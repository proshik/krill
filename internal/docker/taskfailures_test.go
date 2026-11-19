package docker

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
)

func TestCountTaskFailures(t *testing.T) {
	since := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	task := func(svc string, st swarm.TaskState, at time.Time, err ...string) swarm.Task {
		t := swarm.Task{ServiceID: svc, Status: swarm.TaskStatus{State: st, Timestamp: at}}
		if len(err) > 0 {
			t.Status.Err = err[0]
		}
		return t
	}
	// The error texts are what Swarm 28 reported on a throwaway two-node
	// cluster: a crash, then `systemctl restart docker` on the worker.
	tasks := []swarm.Task{
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour), "task: non-zero exit (1)"),     // crash: counts
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour), "No such container: s1.1.abc"), // dockerd restarted: planned
		task("s2", swarm.TaskStateFailed, since.Add(time.Hour), "No such container: s2.1.def"),
		task("s1", swarm.TaskStateRejected, since.Add(2*time.Hour)), // rejected: counts
		task("s1", swarm.TaskStateFailed, since.Add(-time.Minute)),  // before the window
		task("s1", swarm.TaskStateShutdown, since.Add(time.Hour)),   // replaced by a deploy
		task("s1", swarm.TaskStateRunning, since.Add(time.Hour)),
		task("s2", swarm.TaskStateComplete, since.Add(time.Hour)),
		task("gone", swarm.TaskStateFailed, since.Add(time.Hour)), // service not listed
	}
	got := countTaskFailures(tasks, map[string]string{"s1": "krill-1", "s2": "krill-2"}, since)
	if len(got) != 1 || got["krill-1"] != 2 {
		t.Fatalf("countTaskFailures = %v, want krill-1: 2", got)
	}
}
