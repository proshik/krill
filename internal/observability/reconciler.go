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
	networksCooldown  = time.Minute
)

// Status is what the settings page shows about the agent.
type Status struct {
	Apps       docker.ServiceState
	AppsErr    string
	AppsWanted bool
	Busy       bool
	LastRun    time.Time
	LastErr    string
	Service    docker.ServiceState
	StateErr   string
	Coverage   Coverage
}

// Reconciler runs Reconcile in the background whenever the settings change,
// and once at startup, so a request never waits on an image pull.
type Reconciler struct {
	load     func(context.Context) (Settings, error)
	apply    func(context.Context, Settings) (Coverage, error)
	state    func(context.Context, string) (docker.ServiceState, error)
	debounce time.Duration
	trigger  chan struct{}

	mu               sync.Mutex
	busy             bool
	lastRun          time.Time
	lastErr          string
	coverage         Coverage
	appsWanted       bool
	networksCooldown time.Duration
	networksPending  bool
	settingsPending  bool
}

// NewReconciler wires a reconciler to the engine, the settings loader, the
// node-name resolver (see RenderNodeConfig) and the base overlay network the
// agent attaches to. nodes is only consulted while the agent is enabled:
// teardown needs no config, so a broken node lookup must never block turning
// the agent off (see LoadForReconcile's own reasoning for the same shape).
func NewReconciler(eng Engine, load func(context.Context) (Settings, error),
	nodes func(context.Context) ([]NodeName, error), apps func(context.Context) (AppsInput, error), network string) *Reconciler {
	return &Reconciler{
		load: load,
		apply: func(ctx context.Context, s Settings) (Coverage, error) {
			if !s.Enabled {
				return Reconcile(ctx, eng, s, network, nil, AppsInput{})
			}
			ns, err := nodes(ctx)
			if err != nil {
				return Coverage{}, fmt.Errorf("list cluster nodes: %w", err)
			}
			ai, err := apps(ctx)
			if err != nil {
				ai.Unavailable = fmt.Errorf("load apps collector input: %w", err)
			}
			return Reconcile(ctx, eng, s, network, ns, ai)
		},
		state:            eng.ServiceState,
		debounce:         reconcileDebounce,
		networksCooldown: networksCooldown,
		trigger:          make(chan struct{}, 1),
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
	r.settingsPending = true
	r.enqueueLocked()
}

func (r *Reconciler) enqueueLocked() {
	r.busy = true
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// TriggerNetworks coalesces organization-network changes with a cooldown.
func (r *Reconciler) TriggerNetworks() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.networksPending = true
	r.enqueueLocked()
}

// Run serves triggers; settings changes can interrupt a network cooldown.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
		}
		timer := time.NewTimer(r.debounce)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		for {
			r.mu.Lock()
			delay := time.Duration(0)
			if r.networksPending && !r.settingsPending && !r.lastRun.IsZero() {
				delay = r.networksCooldown - time.Since(r.lastRun)
			}
			if delay <= 0 {
				r.networksPending, r.settingsPending = false, false
				select {
				case <-r.trigger:
				default:
				}
				r.mu.Unlock()
				break
			}
			r.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-r.trigger:
				timer.Stop()
			case <-timer.C:
			}
		}
		r.pass(ctx)
	}
}

func (r *Reconciler) pass(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	s, err := r.load(pctx)
	var cov Coverage
	if err == nil {
		cov, err = r.apply(pctx, s)
	}
	if err != nil {
		slog.Error("observability reconcile failed", "err", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appsWanted = s.Enabled && s.Metrics.Configured()
	r.lastRun = time.Now()
	r.lastErr = ""
	// cov is the zero Coverage on a real failure (Reconcile's error paths all
	// return it), so this also clears any earlier partial-coverage note once
	// the pass itself fails outright — LastErr is what the page shows then.
	r.coverage = cov
	if err != nil {
		r.lastErr = err.Error()
	}
	r.busy = len(r.trigger) > 0
}

// Status reports the last pass and the agent's current tasks.
func (r *Reconciler) Status(ctx context.Context) Status {
	r.mu.Lock()
	st := Status{Busy: r.busy, LastRun: r.lastRun, LastErr: r.lastErr, Coverage: r.coverage, AppsWanted: r.appsWanted}
	r.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	svc, err := r.state(sctx, NodeServiceName)
	if err != nil {
		st.StateErr = err.Error()
	} else {
		st.Service = svc
	}
	apps, err := r.state(sctx, AppsServiceName)
	if err != nil {
		st.AppsErr = err.Error()
	} else {
		st.Apps = apps
	}
	return st
}
