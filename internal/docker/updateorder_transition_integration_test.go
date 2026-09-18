//go:build integration

package docker

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
)

// TestUpdateOrderChangeAppliesInSameRollout answers the question the DB
// single-writer fix's "an existing instance protects itself at its next
// deploy" claim depends on: an existing db_instances service is running under
// its old spec, which has UpdateStopFirst false (start-first) because the
// field didn't exist yet. The very first deploy that adds UpdateStopFirst:
// true submits a NEW ServiceSpec — including a new UpdateConfig.Order — as
// part of the SAME ServiceUpdate call that also changes the container
// command. Does swarmkit roll THAT ONE transition out under the order the
// replacement task is defined with, or under the order the task being
// replaced was actually running under? If it's the latter, the new task
// starts before the old one stops — exactly the double-postmaster overlap
// this whole fix exists to close — during the one deploy that's supposed to
// close it. This has to be answered empirically: nothing in the docker/swarm
// client API documents which side of an in-flight UpdateConfig.Order change
// wins.
func TestUpdateOrderChangeAppliesInSameRollout(t *testing.T) {
	eng, err := NewEngine("")
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	e, ok := eng.(*dockerEngine)
	if !ok {
		t.Fatalf("expected *dockerEngine, got %T", eng)
	}
	ctx := context.Background()
	if _, err := e.NetworkEnsure(ctx, "krill-net"); err != nil {
		t.Skipf("swarm/docker unavailable: %v (run: docker swarm init)", err)
	}

	const name = "krill-it-order-transition"
	t.Cleanup(func() { _ = e.ServiceRemove(context.Background(), name) })

	// Command runs a shell that traps SIGTERM and exits immediately, rather
	// than a bare "sleep": Linux specially suppresses the default disposition
	// (for SIGTERM, "terminate the process") for a process that is PID 1 of
	// its own PID namespace UNLESS it has an explicit signal handler
	// installed (signal(7)) — so a bare "sleep 3600" as PID 1 would actually
	// IGNORE ContainerStop's SIGTERM, and the daemon would have to wait out
	// the full stop grace period before falling back to SIGKILL (an earlier
	// version of this test used a bare "sleep" and took ~17s per redeploy for
	// exactly that reason). The trap gives PID 1 an explicit handler, so the
	// shell exits the instant it's signaled, making the redeploy fast.
	base := ServiceSpec{
		Name:     name,
		Image:    "busybox:latest",
		Command:  []string{"sh", "-c", `trap 'exit 0' TERM; sleep 3600 & wait`},
		Replicas: 1,
		Network:  "krill-net",
		// UpdateStopFirst left at its zero value: this is the start-first spec
		// an existing DB instance is already deployed under, before the fix's
		// first stop-first deploy ever touches it.
	}
	if err := e.ServiceDeploy(ctx, base); err != nil {
		t.Fatalf("initial deploy: %v", err)
	}
	oldID := waitForNewServiceContainer(t, e, ctx, name, "", 60*time.Second)
	waitForContainerRunningState(t, e, ctx, oldID, 60*time.Second)

	// The transition deploy: same spec, only UpdateStopFirst flips to true —
	// exactly what an existing DB instance's next Deploy, version change, or
	// node move will submit (DB instances have no Reload; Start/Stop don't
	// resubmit the spec at all).
	next := base
	next.UpdateStopFirst = true
	if err := e.ServiceDeploy(ctx, next); err != nil {
		t.Fatalf("transition deploy: %v", err)
	}

	// Poll both containers together so lastOldState is always the freshest
	// snapshot available at the instant the new container is first observed
	// running — the two checks below are as close to simultaneous as this API
	// allows.
	var lastOldState *container.State
	var newID string
	var newState *container.State
	deadline := time.Now().Add(90 * time.Second)
	for newState == nil {
		if insp, ierr := e.cli.ContainerInspect(ctx, oldID); ierr == nil {
			lastOldState = insp.State
		}
		tasks, terr := e.cli.TaskList(ctx, swarm.TaskListOptions{
			Filters: filters.NewArgs(
				filters.Arg("service", name),
				filters.Arg("desired-state", "running"),
			),
		})
		if terr != nil {
			t.Fatalf("TaskList: %v", terr)
		}
		for _, tk := range tasks {
			if tk.Status.ContainerStatus == nil {
				continue
			}
			id := tk.Status.ContainerStatus.ContainerID
			if id == "" || id == oldID {
				continue
			}
			if insp, ierr := e.cli.ContainerInspect(ctx, id); ierr == nil && insp.State != nil && insp.State.Running {
				newID = id
				newState = insp.State
				break
			}
		}
		if newState == nil {
			if time.Now().After(deadline) {
				t.Fatalf("new container for %s never reached running within %s", name, 90*time.Second)
			}
			time.Sleep(150 * time.Millisecond)
		}
	}

	if lastOldState == nil {
		t.Fatal("never managed to inspect the old container before it disappeared — inconclusive, rerun")
	}
	if lastOldState.Running {
		t.Fatalf("BLOCKED: old container %s was still RUNNING (status=%q) at the moment new container %s was already "+
			"running — swarmkit applied the PREVIOUS start-first order for the very rollout that introduces "+
			"UpdateStopFirst, not the new one carried by the submitted spec. An in-place UpdateStopFirst flip is NOT "+
			"sufficient to protect an already-deployed instance; report to the owner — an explicit transition step "+
			"(e.g. scale to 0 replicas before the first stop-first deploy) is required, and that is the owner's call.",
			oldID, lastOldState.Status, newID)
	}
	oldFinishedAt, ok := parseDockerTime(lastOldState.FinishedAt)
	if !ok {
		t.Fatalf("old container %s has no FinishedAt despite Running=false (status=%q) — inconclusive, rerun", oldID, lastOldState.Status)
	}
	newStartedAt, ok := parseDockerTime(newState.StartedAt)
	if !ok {
		t.Fatalf("new container %s has no StartedAt despite Running=true — inconclusive, rerun", newID)
	}
	if !oldFinishedAt.Before(newStartedAt) {
		t.Fatalf("BLOCKED: old container finished at %s, which is NOT before the new container's start at %s", oldFinishedAt, newStartedAt)
	}
	t.Logf("confirmed: old task finished at %s, new task started at %s — swarmkit applied the NEW stop-first order in the same rollout that introduced it", oldFinishedAt, newStartedAt)
}

// waitForNewServiceContainer polls the service's running-desired tasks until
// one reports a container ID other than exclude, and returns it.
func waitForNewServiceContainer(t *testing.T, e *dockerEngine, ctx context.Context, service, exclude string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		tasks, err := e.cli.TaskList(ctx, swarm.TaskListOptions{
			Filters: filters.NewArgs(
				filters.Arg("service", service),
				filters.Arg("desired-state", "running"),
			),
		})
		if err != nil {
			t.Fatalf("TaskList: %v", err)
		}
		for _, tk := range tasks {
			if tk.Status.ContainerStatus == nil {
				continue
			}
			if id := tk.Status.ContainerStatus.ContainerID; id != "" && id != exclude {
				return id
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no container for service %s (excluding %q) within %s", service, exclude, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// waitForContainerRunningState polls a container until docker reports it
// running and returns its inspected State.
func waitForContainerRunningState(t *testing.T, e *dockerEngine, ctx context.Context, containerID string, timeout time.Duration) *container.State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		insp, err := e.cli.ContainerInspect(ctx, containerID)
		if err == nil && insp.State != nil && insp.State.Running {
			return insp.State
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s did not reach running within %s", containerID, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// parseDockerTime parses a docker RFC3339Nano timestamp field (StartedAt/
// FinishedAt), reporting ok=false for the unset zero value
// ("0001-01-01T00:00:00Z").
func parseDockerTime(s string) (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || ts.IsZero() || ts.Year() <= 1 {
		return time.Time{}, false
	}
	return ts, true
}
