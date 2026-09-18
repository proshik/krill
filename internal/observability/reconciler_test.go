package observability

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

type countingApply struct {
	mu    sync.Mutex
	calls int
	err   error
	done  chan struct{}
}

func (c *countingApply) apply(context.Context, Settings) error {
	c.mu.Lock()
	c.calls++
	err := c.err
	c.mu.Unlock()
	c.done <- struct{}{}
	return err
}

func noNodes(context.Context) ([]NodeName, error) { return nil, nil }

func newTestReconciler(load func(context.Context) (Settings, error), a *countingApply) *Reconciler {
	r := NewReconciler(newFakeEngine(), load, noNodes, "krill-net")
	r.apply = a.apply
	r.debounce = 20 * time.Millisecond
	r.state = func(context.Context) (docker.ServiceState, error) {
		return docker.ServiceState{Found: true, Running: 2, Desired: 3, Failed: 1}, nil
	}
	return r
}

func waitDone(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("pass did not run")
	}
}

func TestReconcilerDebounces(t *testing.T) {
	a := &countingApply{done: make(chan struct{}, 10)}
	r := newTestReconciler(func(context.Context) (Settings, error) { return fullSettings(), nil }, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.Trigger()
	r.Trigger()
	r.Trigger()
	if !r.Status(ctx).Busy {
		t.Error("not busy right after Trigger")
	}
	waitDone(t, a.done)
	time.Sleep(100 * time.Millisecond)
	a.mu.Lock()
	calls := a.calls
	a.mu.Unlock()
	if calls != 1 {
		t.Errorf("passes = %d, want 1", calls)
	}
	st := r.Status(ctx)
	if st.Busy || st.LastRun.IsZero() || st.LastErr != "" {
		t.Errorf("status after a good pass = %+v", st)
	}
	if st.Service.Running != 2 || st.Service.Desired != 3 || st.Service.Failed != 1 {
		t.Errorf("service state = %+v", st.Service)
	}
}

func TestReconcilerReportsErrors(t *testing.T) {
	a := &countingApply{done: make(chan struct{}, 10)}
	loadErr := errors.New("metrics password: secret: value is encrypted but cannot be decrypted")
	fail := true
	var mu sync.Mutex
	load := func(context.Context) (Settings, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return Settings{}, loadErr
		}
		return fullSettings(), nil
	}
	r := newTestReconciler(load, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	r.Trigger()
	deadline := time.Now().Add(2 * time.Second)
	for r.Status(ctx).LastErr == "" {
		if time.Now().After(deadline) {
			t.Fatal("load error never reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.mu.Lock()
	calls := a.calls
	a.mu.Unlock()
	if calls != 0 {
		t.Error("Reconcile ran with settings that failed to load")
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	a.mu.Lock()
	a.err = errors.New("deploy failed")
	a.mu.Unlock()
	r.Trigger()
	waitDone(t, a.done)
	time.Sleep(50 * time.Millisecond)
	if got := r.Status(ctx).LastErr; got != "deploy failed" {
		t.Errorf("LastErr = %q", got)
	}
}

func TestReconcilerStateError(t *testing.T) {
	r := NewReconciler(newFakeEngine(), nil, noNodes, "krill-net")
	r.state = func(context.Context) (docker.ServiceState, error) {
		return docker.ServiceState{}, errors.New("daemon down")
	}
	if st := r.Status(context.Background()); st.StateErr != "daemon down" {
		t.Errorf("status = %+v", st)
	}
}

// TestReconcilerBusyStaysTrueWithBufferedTrigger pins the invariant the fix
// for the Trigger/pass race relies on: Trigger sets busy=true and buffers
// its channel send under the same lock, so by the time it returns the
// buffered trigger and Busy=true always agree. Calling pass directly right
// after Trigger (bypassing Run's own receive) reproduces the exact window
// the finding described — a pass finishing while a trigger is still
// buffered — deterministically, with no goroutines or timing involved: if
// Trigger's two effects were not atomic, pass would see an empty channel
// here and incorrectly clear Busy.
func TestReconcilerBusyStaysTrueWithBufferedTrigger(t *testing.T) {
	a := &countingApply{done: make(chan struct{}, 10)}
	r := newTestReconciler(func(context.Context) (Settings, error) { return fullSettings(), nil }, a)
	ctx := context.Background()

	r.Trigger()
	r.pass(ctx) // the trigger is still buffered: Run has not drained it
	waitDone(t, a.done)

	if st := r.Status(ctx); !st.Busy {
		t.Error("Busy went false while a trigger was still buffered")
	}
}

// Turning the agent off must work even when a stored password no longer
// decrypts (a rotated KRILL_SECRET_KEY): the reconciler's loader must not
// decrypt a disabled configuration.
func TestReconcilerTearsDownDisabledWithUndecryptablePassword(t *testing.T) {
	ctx := context.Background()
	q := db.New(testutil.NewTestDB(t))
	secret.Init("old-key")
	defer secret.Init("")
	if err := Save(ctx, q, Input{Logs: TargetInput{URL: "https://l/x", User: "u", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	secret.Init("new-key")
	eng := newFakeEngine()
	eng.labels = map[string]string{specHashLabel: "x"} // an agent is running
	r := NewReconciler(eng, func(c context.Context) (Settings, error) { return LoadForReconcile(c, q) }, noNodes, "krill-net")
	r.pass(ctx)
	if st := r.Status(ctx); st.LastErr != "" {
		t.Errorf("pass failed: %s", st.LastErr)
	}
	if eng.removed != 1 {
		t.Errorf("agent not removed: removed=%d", eng.removed)
	}
}

// TestReconcilerFailsWhenNodeListingFails proves a broken node-name lookup
// (e.g. ListClusterNodes erroring) fails the pass instead of silently
// rendering the config without the node mapping — an intermittent failure
// must not make the agent flap between the mapped and raw-hostname configs.
func TestReconcilerFailsWhenNodeListingFails(t *testing.T) {
	fastConverge(t)
	nodesErr := errors.New("list cluster nodes: connection refused")
	r := NewReconciler(newFakeEngine(),
		func(context.Context) (Settings, error) { return fullSettings(), nil },
		func(context.Context) ([]NodeName, error) { return nil, nodesErr },
		"krill-net")
	r.pass(context.Background())
	st := r.Status(context.Background())
	if st.LastErr == "" || !strings.Contains(st.LastErr, "connection refused") {
		t.Errorf("LastErr = %q, want it to mention the node listing failure", st.LastErr)
	}
}

// TestReconcilerTeardownIgnoresNodeListFailure proves turning the agent off
// never depends on the node-name lookup succeeding: teardown needs no config,
// so a broken cluster-node listing (or an unreachable docker daemon) must not
// block disabling the agent.
func TestReconcilerTeardownIgnoresNodeListFailure(t *testing.T) {
	eng := newFakeEngine()
	eng.labels = map[string]string{specHashLabel: "x"} // an agent is running
	r := NewReconciler(eng,
		func(context.Context) (Settings, error) { return Settings{}, nil }, // disabled
		func(context.Context) ([]NodeName, error) { return nil, errors.New("boom") },
		"krill-net")
	r.pass(context.Background())
	if st := r.Status(context.Background()); st.LastErr != "" {
		t.Errorf("pass failed: %s", st.LastErr)
	}
	if eng.removed != 1 {
		t.Errorf("agent not removed: removed=%d", eng.removed)
	}
}
