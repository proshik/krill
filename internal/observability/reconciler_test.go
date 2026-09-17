package observability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
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

func newTestReconciler(load func(context.Context) (Settings, error), a *countingApply) *Reconciler {
	r := NewReconciler(newFakeEngine(), load, "krill-net")
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
	r := NewReconciler(newFakeEngine(), nil, "krill-net")
	r.state = func(context.Context) (docker.ServiceState, error) { return docker.ServiceState{}, errors.New("daemon down") }
	if st := r.Status(context.Background()); st.StateErr != "daemon down" {
		t.Errorf("status = %+v", st)
	}
}
