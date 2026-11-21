package notify

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
)

// emitter receives health transitions (implemented by *Service).
type emitter interface {
	AppDown(ctx context.Context, appID int64, detail string)
	AppRecovered(ctx context.Context, appID int64)
	DBDown(ctx context.Context, instanceID int64, detail string)
	DBRecovered(ctx context.Context, instanceID int64)
}

// stateEngine is the slice of docker.Engine the watcher needs.
type stateEngine interface {
	ServiceStates(ctx context.Context, names []string) (map[string]docker.ServiceState, error)
}

// watchKey identifies one watched service: apps and database servers have
// independent id sequences.
type watchKey struct {
	db bool
	id int64
}

type appHealth struct {
	everHealthy bool
	downStreak  int
	alerted     bool
}

// Watcher polls service state and emits AppDown/AppRecovered (apps) and
// DBDown/DBRecovered (database servers) on transitions.
type Watcher struct {
	engine   stateEngine
	store    Store
	emit     emitter
	interval time.Duration
	debounce int
	log      *slog.Logger
	state    map[watchKey]*appHealth
	// primed is set after the first full pass. Database servers recorded as
	// running are taken as healthy before it, so an outage already in
	// progress when Krill (re)starts is still reported.
	primed bool
	now    func() time.Time
}

// dbRunningGrace is how long a database server must have been recorded as
// running before it counts as having run even if no poll saw a running task:
// a server that starts and fails between two polls is still reported, while
// a first start (an image pull) gets this long before it is.
const dbRunningGrace = 5 * time.Minute

func NewWatcher(engine stateEngine, store Store, emit emitter, interval time.Duration) *Watcher {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &Watcher{
		engine: engine, store: store, emit: emit,
		interval: interval, debounce: 2, log: slog.Default(),
		state: map[watchKey]*appHealth{}, now: time.Now,
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
	dbs, err := w.store.ListWatchedDBs(ctx)
	if err != nil {
		w.log.Warn("notify watcher: list database servers failed", "err", err)
		return
	}
	if len(apps) == 0 && len(dbs) == 0 {
		w.primed = true // anything created from now on is not primed
		return          // nothing to poll — avoid a pointless full ServiceList round-trip
	}
	names := make([]string, 0, len(apps)+len(dbs))
	for _, a := range apps {
		names = append(names, docker.ServiceName(a.AppID))
	}
	for _, d := range dbs {
		names = append(names, d.Service)
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	states, err := w.engine.ServiceStates(tctx, names)
	if err != nil {
		w.log.Warn("notify watcher: service states failed", "err", err)
		return
	}
	// The first pass that actually evaluates ends the startup window.
	defer func() { w.primed = true }()
	live := make(map[watchKey]struct{}, len(apps)+len(dbs))
	for _, a := range apps {
		k := watchKey{id: a.AppID}
		live[k] = struct{}{}
		st := states[docker.ServiceName(a.AppID)]
		switch w.evaluate(k, st) {
		case transitionDown:
			w.emit.AppDown(ctx, a.AppID, downDetail(st))
		case transitionRecovered:
			w.emit.AppRecovered(ctx, a.AppID)
		}
	}
	for _, d := range dbs {
		k := watchKey{db: true, id: d.InstanceID}
		live[k] = struct{}{}
		st := states[d.Service]
		if d.Status == "migrating" || oplock.Held(oplock.DBInstance(d.Service)) {
			// A node move, a deploy or another operation holding the server
			// stops or restarts it on purpose: nothing it shows meanwhile is an
			// outage or a recovery (a failed move or deploy alerts itself). An
			// alert already sent stays outstanding.
			if h := w.state[k]; h != nil {
				h.downStreak = 0
			}
			continue
		}
		h := w.state[k]
		if h == nil {
			h = &appHealth{}
			w.state[k] = h
		}
		if !h.everHealthy && d.Status == "running" && (!w.primed || w.now().Sub(d.UpdatedAt) >= dbRunningGrace) {
			h.everHealthy = true
		}
		switch w.evaluate(k, st) {
		case transitionDown:
			detail := downDetail(st)
			if d.Node != "" {
				detail += " · " + d.Node
			}
			w.emit.DBDown(ctx, d.InstanceID, detail)
		case transitionRecovered:
			w.emit.DBRecovered(ctx, d.InstanceID)
		}
	}
	w.pruneState(live)
}

// pruneState drops health entries for services no longer watched (deleted),
// so the in-memory state map cannot grow without bound under churn.
func (w *Watcher) pruneState(live map[watchKey]struct{}) {
	if len(w.state) <= len(live) {
		return
	}
	for k := range w.state {
		if _, ok := live[k]; !ok {
			delete(w.state, k)
		}
	}
}

type transition int

const (
	transitionNone transition = iota
	transitionDown
	transitionRecovered
)

// evaluate advances one service's health state and reports the transition to
// announce, if any.
func (w *Watcher) evaluate(k watchKey, st docker.ServiceState) transition {
	h := w.state[k]
	if h == nil {
		h = &appHealth{}
		w.state[k] = h
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
		recovered := h.alerted
		h.downStreak = 0
		h.alerted = false
		if recovered {
			return transitionRecovered
		}
	case down:
		if h.everHealthy {
			h.downStreak++
			if h.downStreak >= w.debounce && !h.alerted {
				h.alerted = true
				return transitionDown
			}
		}
	default: // partial / in transition
		h.downStreak = 0
	}
	return transitionNone
}

func downDetail(st docker.ServiceState) string {
	return strconv.Itoa(st.Running) + "/" + strconv.Itoa(st.Desired) + " tasks running"
}
