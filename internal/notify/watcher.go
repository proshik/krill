package notify

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// emitter receives health transitions (implemented by *Service).
type emitter interface {
	AppDown(ctx context.Context, appID int64, detail string)
	AppRecovered(ctx context.Context, appID int64)
}

// stateEngine is the slice of docker.Engine the watcher needs.
type stateEngine interface {
	ServiceStates(ctx context.Context, names []string) (map[string]docker.ServiceState, error)
}

type appHealth struct {
	everHealthy bool
	downStreak  int
	alerted     bool
}

// Watcher polls service state and emits AppDown/AppRecovered on transitions.
type Watcher struct {
	engine   stateEngine
	store    Store
	emit     emitter
	interval time.Duration
	debounce int
	log      *slog.Logger
	state    map[int64]*appHealth
}

func NewWatcher(engine stateEngine, store Store, emit emitter, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Watcher{
		engine: engine, store: store, emit: emit,
		interval: interval, debounce: 2, log: slog.Default(),
		state: map[int64]*appHealth{},
	}
}

// Run ticks until ctx is canceled.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick performs one poll + transition pass.
func (w *Watcher) tick(ctx context.Context) {
	n, err := w.store.EnabledHealthChannels(ctx)
	if err != nil {
		w.log.Warn("notify watcher: count health channels failed", "err", err)
		return
	}
	if n == 0 {
		return // nobody listening — skip the (cheap but pointless) state poll
	}
	apps, err := w.store.ListWatchedApps(ctx)
	if err != nil {
		w.log.Warn("notify watcher: list apps failed", "err", err)
		return
	}
	if len(apps) == 0 {
		return // nothing to poll — avoid a pointless full ServiceList round-trip
	}
	names := make([]string, 0, len(apps))
	for _, a := range apps {
		names = append(names, docker.ServiceName(a.AppID))
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	states, err := w.engine.ServiceStates(tctx, names)
	if err != nil {
		w.log.Warn("notify watcher: service states failed", "err", err)
		return
	}
	for _, a := range apps {
		w.evaluate(ctx, a.AppID, states[docker.ServiceName(a.AppID)])
	}
	w.pruneState(apps)
}

// pruneState drops per-app health entries for apps no longer watched (deleted),
// so the in-memory state map cannot grow without bound under app churn.
func (w *Watcher) pruneState(apps []WatchedApp) {
	if len(w.state) <= len(apps) {
		return
	}
	live := make(map[int64]struct{}, len(apps))
	for _, a := range apps {
		live[a.AppID] = struct{}{}
	}
	for id := range w.state {
		if _, ok := live[id]; !ok {
			delete(w.state, id)
		}
	}
}

func (w *Watcher) evaluate(ctx context.Context, appID int64, st docker.ServiceState) {
	h := w.state[appID]
	if h == nil {
		h = &appHealth{}
		w.state[appID] = h
	}
	healthy := st.Found && st.Desired > 0 && st.Running >= st.Desired
	down := st.Found && st.Desired > 0 && (st.Running == 0 || (st.Failed >= 3 && st.Running < st.Desired))
	inactive := !st.Found || st.Desired == 0

	switch {
	case inactive:
		h.downStreak = 0
		h.alerted = false // intentional stop → no recovery message
	case healthy:
		h.everHealthy = true
		if h.alerted {
			w.emit.AppRecovered(ctx, appID)
		}
		h.downStreak = 0
		h.alerted = false
	case down:
		if h.everHealthy {
			h.downStreak++
			if h.downStreak >= w.debounce && !h.alerted {
				w.emit.AppDown(ctx, appID, downDetail(st))
				h.alerted = true
			}
		}
	default: // partial / in transition
		h.downStreak = 0
	}
}

func downDetail(st docker.ServiceState) string {
	return strconv.Itoa(st.Running) + "/" + strconv.Itoa(st.Desired) + " tasks running"
}
