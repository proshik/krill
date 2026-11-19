package docker

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
)

func TestCountTaskFailures(t *testing.T) {
	since := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	task := func(svc string, st swarm.TaskState, at time.Time) swarm.Task {
		return swarm.Task{ServiceID: svc, Status: swarm.TaskStatus{State: st, Timestamp: at}}
	}
	tasks := []swarm.Task{
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour)),     // crash: counts
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
