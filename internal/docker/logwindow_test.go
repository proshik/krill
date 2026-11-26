package docker

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"
)

func TestLogsOptionsWindow(t *testing.T) {
	since := time.Date(2026, 11, 26, 14, 0, 0, 5, time.UTC)
	until := since.Add(10 * time.Minute)

	o := logsOptions(LogWindow{Tail: 200})
	if o.Tail != "200" || o.Since != "" || o.Until != "" || o.Follow || !o.Timestamps || !o.Details {
		t.Fatalf("plain tail: %+v", o)
	}
	o = logsOptions(LogWindow{Since: since, Until: until, Tail: TailAll})
	if o.Tail != "all" {
		t.Fatalf("TailAll must read the whole window, got %q", o.Tail)
	}
	if o := logsOptions(LogWindow{Follow: true}); o.Tail != "0" {
		t.Fatalf("tail 0 must stay 0 (new lines only), got %q", o.Tail)
	}
	if o.Since != "1795701600.000000005" || o.Until != "1795702200.000000005" {
		t.Fatalf("bounds: since %q until %q", o.Since, o.Until)
	}
	if o := logsOptions(LogWindow{Follow: true, Since: since, Tail: TailAll}); !o.Follow || o.Since == "" {
		t.Fatalf("follow: %+v", o)
	}
}

func TestInWindowSkipsTasksOutsideIt(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 26, 14, m, 0, 0, time.UTC) }
	task := func(id string, state swarm.TaskState, created, changed int) swarm.Task {
		tk := swarm.Task{ID: id, Status: swarm.TaskStatus{State: state, Timestamp: at(changed)}}
		tk.Meta.CreatedAt = at(created)
		return tk
	}
	tasks := []swarm.Task{
		task("ended-before", swarm.TaskStateShutdown, 1, 5),
		task("ended-inside", swarm.TaskStateFailed, 2, 12),
		task("running-old", swarm.TaskStateRunning, 0, 1),
		task("created-after", swarm.TaskStateRunning, 40, 40),
	}
	var ids []string
	for _, tk := range inWindow(tasks, at(10), at(30)) {
		ids = append(ids, tk.ID)
	}
	if got := strings.Join(ids, ","); got != "ended-inside,running-old" {
		t.Fatalf("kept %s", got)
	}
	if got := inWindow(tasks, time.Time{}, time.Time{}); len(got) != len(tasks) {
		t.Fatalf("no window must keep every task, kept %d", len(got))
	}
}

func TestHealthOfKeepsLatestProbeWithoutOutput(t *testing.T) {
	end := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)
	h := healthOf(&container.Health{
		Status:        "unhealthy",
		FailingStreak: 3,
		Log: []*container.HealthcheckResult{
			{ExitCode: 0, Output: "ok"},
			{ExitCode: 1, Output: "token=hunter2", End: end},
		},
	})
	if h != (ContainerHealth{Status: "unhealthy", FailingStreak: 3, LastExitCode: 1, LastAt: end}) {
		t.Fatalf("health: %+v", h)
	}
	if h := healthOf(&container.Health{Status: "starting"}); h != (ContainerHealth{Status: "starting"}) {
		t.Fatalf("no probe yet: %+v", h)
	}
}
