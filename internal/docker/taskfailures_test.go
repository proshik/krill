package docker

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
)

// Each error text Swarm was seen to write, on production and on a throwaway
// docker-in-docker cluster, classified on its own.
func TestPlannedStop(t *testing.T) {
	for err, want := range map[string]bool{
		"task: non-zero exit (143)":                                  true, // dockerd restart, JVM app (production)
		"No such container: krill-1.1.8ms6ushaat442ismpnx1vh271":     true, // dockerd restart (dind)
		"task: non-zero exit (1)":                                    false,
		"task: non-zero exit (137)":                                  false, // OOM, or a stop past the grace period
		"task: non-zero exit (130)":                                  false,
		"task: non-zero exit (143): dockerexec: unhealthy container": false,
		"task: non-zero exit (137): dockerexec: unhealthy container": false,
		"task: non-zero exit (1430)":                                 false,
		"no suitable node (scheduling constraints not satisfied)":    false,
	} {
		if got := plannedStop(err); got != want {
			t.Errorf("plannedStop(%q) = %v, want %v", err, got, want)
		}
	}
}

func TestCountTaskFailures(t *testing.T) {
	since := time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)
	task := func(svc string, st swarm.TaskState, at time.Time, err ...string) swarm.Task {
		t := swarm.Task{ServiceID: svc, Status: swarm.TaskStatus{State: st, Timestamp: at}}
		if len(err) > 0 {
			t.Status.Err = err[0]
		}
		return t
	}
	// The error texts are what Swarm reported: on production (a JVM app
	// stopped by `systemctl restart docker` on its worker exits 143) and on a
	// throwaway docker-in-docker cluster (crash, OOM, healthcheck, restart).
	tasks := []swarm.Task{
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour), "task: non-zero exit (1)"),                                    // crash: counts
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour), "task: non-zero exit (137)"),                                  // OOM kill: counts
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour), "task: non-zero exit (143): dockerexec: unhealthy container"), // healthcheck kill of a SIGTERM-aware app: counts
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour), "task: non-zero exit (143)"),                                  // dockerd restarted (production): planned
		task("s1", swarm.TaskStateFailed, since.Add(time.Hour), "No such container: s1.1.abc"),                                // dockerd restarted (dind): planned
		task("s2", swarm.TaskStateFailed, since.Add(time.Hour), "No such container: s2.1.def"),
		task("s1", swarm.TaskStateRejected, since.Add(2*time.Hour)), // rejected: counts
		task("s1", swarm.TaskStateFailed, since.Add(-time.Minute)),  // before the window
		task("s1", swarm.TaskStateShutdown, since.Add(time.Hour)),   // replaced by a deploy
		task("s1", swarm.TaskStateRunning, since.Add(time.Hour)),
		task("s2", swarm.TaskStateComplete, since.Add(time.Hour)),
		task("gone", swarm.TaskStateFailed, since.Add(time.Hour)), // service not listed
	}
	got := countTaskFailures(tasks, map[string]string{"s1": "krill-1", "s2": "krill-2"}, since)
	if len(got) != 1 || got["krill-1"] != 4 {
		t.Fatalf("countTaskFailures = %v, want krill-1: 4", got)
	}
}
