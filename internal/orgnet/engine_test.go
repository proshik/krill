package orgnet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

type fakeStater struct {
	states map[string]docker.ServiceState
	calls  int
	err    error
	// errCalls fails that many calls before answering normally.
	errCalls int
	// after N calls, every service reported switches to running
	upAfter int
}

func (f *fakeStater) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.calls <= f.errCalls {
		return nil, errors.New("transient")
	}
	if f.upAfter > 0 && f.calls >= f.upAfter {
		out := map[string]docker.ServiceState{}
		for _, n := range names {
			out[n] = docker.ServiceState{Found: true, Running: 1, Desired: 1}
		}
		return out, nil
	}
	return f.states, nil
}

// A stopped app (a service scaled to zero) and a never-deployed app (no service
// at all) must be left alone: an upgrade is not a reason to start something the
// operator stopped, and redeploying an app that never ran only writes a failed
// deployment row.
func TestRunningAppsFilterSkipsStoppedAndUndeployedApps(t *testing.T) {
	st := &fakeStater{states: map[string]docker.ServiceState{
		docker.ServiceName(1): {Found: true, Running: 1, Desired: 1}, // running
		docker.ServiceName(2): {Found: true, Running: 0, Desired: 0}, // stopped
		// id 3 has no service at all: never deployed
	}}
	got, err := RunningAppsFilter(st)(context.Background(), []int64{1, 2, 3})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("only the running app may move, got %v", got)
	}
	if st.calls != 1 {
		t.Fatalf("the filter must read every state in one call, got %d calls", st.calls)
	}
}

func TestRunningAppsFilterReportsAnEngineFailure(t *testing.T) {
	st := &fakeStater{err: errors.New("boom")}
	if _, err := RunningAppsFilter(st)(context.Background(), []int64{1}); err == nil {
		t.Fatal("an engine failure must be reported, not silently treated as 'nothing runs'")
	}
}

func TestWaitServicesRunningReturnsWhenEverythingIsUp(t *testing.T) {
	st := &fakeStater{upAfter: 3, states: map[string]docker.ServiceState{
		"db-a": {Found: true, Running: 0, Desired: 1},
	}}
	err := WaitServicesRunning(context.Background(), st, []string{"db-a"}, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if st.calls < 3 {
		t.Fatalf("want polling until up, got %d calls", st.calls)
	}
}

// The wait is bounded: a database that never comes back must not hold startup
// (and every app behind it) forever.
func TestWaitServicesRunningTimesOut(t *testing.T) {
	st := &fakeStater{states: map[string]docker.ServiceState{
		"db-a": {Found: true, Running: 0, Desired: 1},
	}}
	err := WaitServicesRunning(context.Background(), st, []string{"db-a"}, time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("want ErrWaitTimeout, got %v", err)
	}
}

func TestWaitServicesRunningWithNoNamesIsANoop(t *testing.T) {
	st := &fakeStater{}
	if err := WaitServicesRunning(context.Background(), st, nil, time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if st.calls != 0 {
		t.Fatalf("no names must mean no engine call, got %d", st.calls)
	}
}

// One failed state read is not a verdict on the databases. Returning on it would
// cost the organization its pass for a Docker blip; the wait keeps polling.
func TestWaitServicesRunningSurvivesATransientEngineError(t *testing.T) {
	st := &fakeStater{errCalls: 2, upAfter: 3}
	err := WaitServicesRunning(context.Background(), st, []string{"db-a"}, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("a transient engine error must not end the wait, got %v", err)
	}
	if st.calls < 3 {
		t.Fatalf("want polling past the errors, got %d calls", st.calls)
	}
}

// An engine that never answers still ends at the deadline, as a timeout.
func TestWaitServicesRunningTimesOutOnAPersistentEngineError(t *testing.T) {
	st := &fakeStater{err: errors.New("boom")}
	err := WaitServicesRunning(context.Background(), st, []string{"db-a"}, time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("want ErrWaitTimeout, got %v", err)
	}
	if st.calls < 2 {
		t.Fatalf("a failed read must be retried, got %d calls", st.calls)
	}
}

// A database the operator stopped must not be started by an upgrade, and one
// that cannot start must not be waited on. But it must still move: Start only
// scales the existing service, so an instance left on the shared network would
// come back up there. It is parked — at its current replica count — instead.
func TestRunningInstancesFilterParksStoppedInstances(t *testing.T) {
	rows := map[int64]InstanceRow{
		1: {AppName: "db-running", Status: "running"},
		2: {AppName: "db-stopped", Status: "idle"},
		3: {AppName: "db-never", Status: "idle"},
		4: {AppName: "db-scaled-down", Status: "running"},
		5: {AppName: "db-row-stopped", Status: "idle"},
		6: {AppName: "db-last-deploy-failed", Status: "error"},
	}
	st := &fakeStater{states: map[string]docker.ServiceState{
		"db-running":            {Found: true, Running: 1, Desired: 1},
		"db-stopped":            {Found: true, Running: 0, Desired: 0},
		"db-scaled-down":        {Found: true, Running: 0, Desired: 0},
		"db-row-stopped":        {Found: true, Running: 1, Desired: 1},
		"db-last-deploy-failed": {Found: true, Running: 1, Desired: 1},
		// db-never has no service at all
	}}
	lookup := func(_ context.Context, id int64) (InstanceRow, error) { return rows[id], nil }

	plan, err := RunningInstancesFilter(st, lookup)(context.Background(), []int64{1, 2, 3, 4, 5, 6})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(plan.Running) != 2 || plan.Running[0] != 1 || plan.Running[1] != 6 {
		t.Fatalf("running = %v, want [1 6]", plan.Running)
	}
	want := []ParkedInstance{{ID: 2, Replicas: 0}, {ID: 4, Replicas: 0}, {ID: 5, Replicas: 1}}
	if len(plan.Parked) != len(want) {
		t.Fatalf("parked = %v, want %v", plan.Parked, want)
	}
	for i := range want {
		if plan.Parked[i] != want[i] {
			t.Fatalf("parked = %v, want %v", plan.Parked, want)
		}
	}
	if st.calls != 1 {
		t.Fatalf("the filter must read every state in one call, got %d calls", st.calls)
	}
}

func TestRunningInstancesFilterReportsFailures(t *testing.T) {
	ok := func(context.Context, int64) (InstanceRow, error) { return InstanceRow{AppName: "db"}, nil }
	if _, err := RunningInstancesFilter(&fakeStater{err: errors.New("boom")}, ok)(context.Background(), []int64{1}); err == nil {
		t.Fatal("an engine failure must be reported, not silently treated as 'nothing runs'")
	}
	bad := func(context.Context, int64) (InstanceRow, error) { return InstanceRow{}, errors.New("boom") }
	if _, err := RunningInstancesFilter(&fakeStater{}, bad)(context.Background(), []int64{1}); err == nil {
		t.Fatal("a failed row lookup must be reported")
	}
}
