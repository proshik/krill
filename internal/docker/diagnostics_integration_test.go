//go:build integration

package docker_test

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// The agent API's status and log reads against a real Swarm: task history
// carries the container and creation time, a probe reads health and memory
// on the task's node, and a log window reads from a start time — both as a
// task-by-task backlog and as a following stream.
func TestDiagnosticsAgainstARealService(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	skipIfNoSwarm(t, e)
	th, ok1 := e.(docker.TaskHistory)
	pr, ok2 := e.(docker.ContainerProber)
	wl, ok3 := e.(docker.WindowedLogs)
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("the real engine must implement TaskHistory, ContainerProber and WindowedLogs")
	}
	ctx := context.Background()
	const name = "krill-it-diag"
	const image = "busybox:1.37.0@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"
	t.Cleanup(func() { _ = e.ServiceRemove(context.Background(), name) })
	if err := e.ImagePull(ctx, image, io.Discard); err != nil {
		t.Fatalf("pull: %v", err)
	}
	start := time.Now()
	spec := docker.ServiceSpec{
		Name: name, Image: image, Replicas: 1, Network: "krill-net",
		Command:          []string{"sh", "-c", "echo first-line; sleep 15; echo second-line; exec sleep 600"},
		MemoryLimitBytes: 64 << 20,
		Healthcheck:      &docker.HealthcheckSpec{Test: []string{"CMD-SHELL", "true"}, Interval: time.Second, Timeout: time.Second, Retries: 3},
	}
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	var task docker.TaskRecord
	deadline := time.Now().Add(90 * time.Second)
	for task.ContainerID == "" {
		if time.Now().After(deadline) {
			t.Fatal("no task with a container")
		}
		recs, err := th.ServiceTaskHistory(ctx, name, 5)
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		for _, r := range recs {
			if r.Desired == "running" && r.ContainerID != "" {
				task = r
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if task.Created.IsZero() || task.NodeID == "" {
		t.Fatalf("task record lacks its creation time or node: %+v", task)
	}

	var probe docker.ContainerProbe
	for deadline = time.Now().Add(30 * time.Second); ; {
		probe, err = pr.ProbeContainer(ctx, task.NodeID, task.ContainerID)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if probe.Running && probe.Health != nil && probe.Health.Status == "healthy" && probe.MemOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe never saw a healthy running container: %+v %+v", probe, probe.Health)
		}
		time.Sleep(time.Second)
	}
	if probe.MemLimitBytes != 64<<20 || probe.MemBytes <= 0 {
		t.Fatalf("memory: %+v", probe)
	}

	// The backlog from the deploy on holds the first line.
	rc, err := wl.ServiceLogsWindow(ctx, name, docker.LogWindow{Since: start.Add(-time.Second), Tail: docker.TailAll})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(b), "first-line") {
		t.Fatalf("window read lacks the first line: %q", b)
	}

	// A following read from now sees the second line arrive.
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	frc, err := wl.ServiceLogsWindow(fctx, name, docker.LogWindow{Since: time.Now(), Tail: docker.TailAll, Follow: true})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer frc.Close()
	sc := bufio.NewScanner(frc)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "first-line") {
			t.Fatalf("a follow from now must not replay older lines: %q", sc.Text())
		}
		if strings.Contains(sc.Text(), "second-line") {
			return
		}
	}
	t.Fatalf("follow ended without the second line: %v", sc.Err())
}
