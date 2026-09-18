package observability

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/proshik/krill/internal/docker"
)

const (
	reconcileDebounce = 2 * time.Second
	passTimeout       = 10 * time.Minute
	stateTimeout      = 5 * time.Second
)

// Status is what the settings page shows about the agent.
type Status struct {
	Busy     bool
	LastRun  time.Time
	LastErr  string
	Service  docker.ServiceState
	StateErr string
}

// Reconciler runs Reconcile in the background whenever the settings change,
// and once at startup, so a request never waits on an image pull.
type Reconciler struct {
	load     func(context.Context) (Settings, error)
	apply    func(context.Context, Settings) error
	state    func(context.Context) (docker.ServiceState, error)
	debounce time.Duration
	trigger  chan struct{}

	mu      sync.Mutex
	busy    bool
	lastRun time.Time
	lastErr string
}

// NewReconciler wires a reconciler to the engine, the settings loader, the
// node-name resolver (see RenderNodeConfig) and the base overlay network the
// agent attaches to. nodes is only consulted while the agent is enabled:
// teardown needs no config, so a broken node lookup must never block turning
// the agent off (see LoadForReconcile's own reasoning for the same shape).
func NewReconciler(eng Engine, load func(context.Context) (Settings, error),
	nodes func(context.Context) ([]NodeName, error), network string) *Reconciler {
	return &Reconciler{
		load: load,
		apply: func(ctx context.Context, s Settings) error {
			if !s.Enabled {
				return Reconcile(ctx, eng, s, network, nil)
			}
			ns, err := nodes(ctx)
			if err != nil {
				return fmt.Errorf("list cluster nodes: %w", err)
			}
			return Reconcile(ctx, eng, s, network, ns)
		},
		state:    func(ctx context.Context) (docker.ServiceState, error) { return eng.ServiceState(ctx, NodeServiceName) },
		debounce: reconcileDebounce,
		trigger:  make(chan struct{}, 1),
	}
}

// Trigger asks for a pass. It never blocks; one pending request is enough,
// because every pass reads the settings fresh. The (non-blocking) channel
// send happens inside the same critical section as setting busy=true: pass
// derives busy from len(r.trigger) under the same lock, so releasing the
// lock between the two would let a finishing pass observe an empty channel
// and report busy=false while this trigger is still in flight to it.
func (r *Reconciler) Trigger() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy = true
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run serves triggers until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.debounce):
		}
		select { // this pass covers whatever arrived during the quiet period
		case <-r.trigger:
		default:
		}
		r.pass(ctx)
	}
}

func (r *Reconciler) pass(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	s, err := r.load(pctx)
	if err == nil {
		err = r.apply(pctx, s)
	}
	if err != nil {
		slog.Error("observability reconcile failed", "err", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastRun = time.Now()
	r.lastErr = ""
	if err != nil {
		r.lastErr = err.Error()
	}
	r.busy = len(r.trigger) > 0
}

// Status reports the last pass and the agent's current tasks.
func (r *Reconciler) Status(ctx context.Context) Status {
	r.mu.Lock()
	st := Status{Busy: r.busy, LastRun: r.lastRun, LastErr: r.lastErr}
	r.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	svc, err := r.state(sctx)
	if err != nil {
		st.StateErr = err.Error()
	} else {
		st.Service = svc
	}
	return st
}
