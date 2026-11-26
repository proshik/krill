//go:build integration

package docker_test

import (
	"bufio"
	"context"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

func skipIfNoSwarm(t *testing.T, e docker.Engine) {
	t.Helper()
	if _, err := e.NetworkEnsure(context.Background(), "krill-net"); err != nil {
		t.Skipf("swarm/docker unavailable: %v (run: docker swarm init)", err)
	}
}

func TestEngineLifecycle(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	skipIfNoSwarm(t, e)
	ctx := context.Background()
	name := "krill-it-nginx"
	t.Cleanup(func() { _ = e.ServiceRemove(context.Background(), name) })

	spec := docker.ServiceSpec{
		Name:     name,
		Image:    "nginx:alpine",
		Replicas: 1,
		Network:  "krill-net",
	}
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Wait for running (up to ~60s).
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err := e.ServiceState(ctx, name)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if st.Found && st.Running >= 1 {
			if st.Global {
				t.Fatalf("a replicated service reads global: %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not reach running: %+v", st)
		}
		time.Sleep(2 * time.Second)
	}

	// Logs are available.
	rc, err := e.ServiceLogs(ctx, name, false, 200)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Scan() // it's enough that the stream reads without panicking
}

// ServiceState marks a global service, whose desired count is its task list:
// the app sheet recounts it from the tasks it shows.
func TestServiceStateGlobal(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	skipIfNoSwarm(t, e)
	ctx := context.Background()
	name := "krill-it-global"
	t.Cleanup(func() { _ = e.ServiceRemove(context.Background(), name) })
	if err := e.ServiceDeploy(ctx, docker.ServiceSpec{Name: name, Image: "nginx:alpine", Global: true, Network: "krill-net"}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err := e.ServiceState(ctx, name)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if st.Found && st.Desired >= 1 {
			if !st.Global {
				t.Fatalf("a global service does not read global: %+v", st)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no task of the global service: %+v", st)
		}
		time.Sleep(time.Second)
	}
}
