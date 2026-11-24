//go:build integration

package docker_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/logparse"
)

// A non-following read of a whole service stopped mid-stream and never
// ended once its tasks' tails were large (seen on a production cluster with
// Docker 29.1.3 and reproduced on a two-node docker:28-dind Swarm: five
// tasks of ~1.2 KB lines, --tail 200, six reads out of six). Krill's
// ServiceLogs must end, with every task's tail. Set KRILL_IT_CONSTRAINT (e.g. node.role==worker) to put the
// tasks on another node than the manager, as on that cluster.
func TestServiceLogsBacklogEndsWithEveryTask(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	skipIfNoSwarm(t, e)
	ctx := context.Background()
	const name = "krill-it-biglogs"
	t.Cleanup(func() { _ = e.ServiceRemove(context.Background(), name) })
	spec := docker.ServiceSpec{
		Name: name, Image: "alpine:3.20", Replicas: 1, Network: "krill-net",
		Command: []string{"sh", "-c", `i=0; while [ $i -lt 300 ]; do echo "line $i $(head -c 900 /dev/urandom | base64 | tr -d '\n')"; i=$((i+1)); done; exec sleep 100000`},
	}
	if c := os.Getenv("KRILL_IT_CONSTRAINT"); c != "" {
		spec.Constraints = []string{c}
	}
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	const runs = 5
	waitPrinted(t, e, name, 1)
	for i := 2; i <= runs; i++ {
		if err := e.ServiceRestart(ctx, name); err != nil {
			t.Fatalf("restart: %v", err)
		}
		waitPrinted(t, e, name, i)
	}

	for attempt := 0; attempt < 3; attempt++ {
		perTask, total, took := readBacklog(t, e, name, 60*time.Second)
		if total != runs*200 || len(perTask) != runs {
			t.Fatalf("attempt %d: %d lines from %d tasks in %v, want %d from %d: %v", attempt, total, len(perTask), took, runs*200, runs, perTask)
		}
	}
}

// waitPrinted waits until the service's n-th task is running and has
// printed its 300 lines.
func waitPrinted(t *testing.T, e docker.Engine, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		perTask, _, _ := readBacklog(t, e, name, 60*time.Second)
		full := 0
		for _, c := range perTask {
			if c == 200 {
				full++
			}
		}
		if full >= n {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("task %d never printed its lines", n)
}

func readBacklog(t *testing.T, e docker.Engine, name string, limit time.Duration) (map[string]int, int, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	rc, err := e.ServiceLogs(ctx, name, false, 200)
	if err != nil {
		t.Fatalf("service logs: %v", err)
	}
	defer rc.Close()
	perTask := map[string]int{}
	total := 0
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		l := logparse.ParseLogLine(sc.Text())
		perTask[l.Task]++
		total++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(fmt.Errorf("read after %v (%d lines): %w", time.Since(start), total, err))
	}
	return perTask, total, time.Since(start)
}
