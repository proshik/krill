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
	// after N calls, every service reported switches to running
	upAfter int
}

func (f *fakeStater) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
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
