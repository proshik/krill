//go:build integration

package docker_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// A task killed for exceeding its memory limit shows up in the history with
// exit code 137 — what the Advanced tab turns into its out-of-memory hint.
func TestServiceTaskHistoryReportsOOMKill(t *testing.T) {
	e, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	skipIfNoSwarm(t, e)
	th, ok := e.(docker.TaskHistory)
	if !ok {
		t.Fatal("the real engine must implement TaskHistory")
	}
	ctx := context.Background()
	const name = "krill-it-oom"
	const image = "busybox:1.37.0@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"
	t.Cleanup(func() { _ = e.ServiceRemove(context.Background(), name) })
	if err := e.ImagePull(ctx, image, io.Discard); err != nil {
		t.Fatalf("pull: %v", err)
	}
	spec := docker.ServiceSpec{
		Name: name, Image: image, Replicas: 1, Network: "krill-net",
		Command:          []string{"sh", "-c", "exec dd if=/dev/zero of=/dev/null bs=96M count=1"},
		MemoryLimitBytes: 16 << 20,
		RestartCondition: "none",
	}
	if err := e.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		tasks, err := th.ServiceTaskHistory(ctx, name, 5)
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		for _, tk := range tasks {
			if tk.ExitCode == 137 {
				if tk.Updated.IsZero() || tk.State == "running" {
					t.Fatalf("an OOM-killed task must be terminated with a timestamp: %+v", tk)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no task with exit 137: %+v", tasks)
		}
		time.Sleep(2 * time.Second)
	}
}
