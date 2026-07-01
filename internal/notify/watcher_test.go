package notify

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

type fakeEmitter struct {
	down      []int64
	recovered []int64
}

func (f *fakeEmitter) AppDown(_ context.Context, appID int64, _ string) { f.down = append(f.down, appID) }
func (f *fakeEmitter) AppRecovered(_ context.Context, appID int64)      { f.recovered = append(f.recovered, appID) }

type scriptEngine struct{ states map[string]docker.ServiceState }

func (s *scriptEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	out := map[string]docker.ServiceState{}
	for _, n := range names {
		out[n] = s.states[n]
	}
	return out, nil
}

func healthyState() docker.ServiceState { return docker.ServiceState{Found: true, Desired: 1, Running: 1} }
func downState() docker.ServiceState    { return docker.ServiceState{Found: true, Desired: 1, Running: 0} }

func newTestWatcher(eng stateEngine, st Store, em emitter) *Watcher {
	w := NewWatcher(eng, st, em, 0)
	w.debounce = 2
	return w
}

func TestWatcherDownAfterDebounceThenRecover(t *testing.T) {
	name := docker.ServiceName(5)
	eng := &scriptEngine{states: map[string]docker.ServiceState{name: healthyState()}}
	st := &fakeStore{health: 1, watched: []WatchedApp{{AppID: 5, OrgID: 7, Name: "web"}}}
	em := &fakeEmitter{}
	w := newTestWatcher(eng, st, em)
	ctx := context.Background()

	w.tick(ctx) // healthy: baseline, no alert
	if len(em.down) != 0 {
		t.Fatal("no alert while healthy")
	}
	eng.states[name] = downState()
	w.tick(ctx) // down streak 1 < debounce 2 → no alert yet
	if len(em.down) != 0 {
		t.Fatalf("debounce not respected: %v", em.down)
	}
	w.tick(ctx) // down streak 2 == debounce → AppDown
	if len(em.down) != 1 || em.down[0] != 5 {
		t.Fatalf("expected one AppDown for app 5, got %v", em.down)
	}
	w.tick(ctx) // still down, already alerted → no duplicate
	if len(em.down) != 1 {
		t.Fatalf("duplicate down alert: %v", em.down)
	}
	eng.states[name] = healthyState()
	w.tick(ctx) // recovered
	if len(em.recovered) != 1 || em.recovered[0] != 5 {
		t.Fatalf("expected one AppRecovered, got %v", em.recovered)
	}
}

func TestWatcherNeverHealthyDoesNotAlert(t *testing.T) {
	name := docker.ServiceName(5)
	eng := &scriptEngine{states: map[string]docker.ServiceState{name: downState()}}
	st := &fakeStore{health: 1, watched: []WatchedApp{{AppID: 5, OrgID: 7, Name: "web"}}}
	em := &fakeEmitter{}
	w := newTestWatcher(eng, st, em)
	for i := 0; i < 5; i++ {
		w.tick(context.Background())
	}
	if len(em.down) != 0 {
		t.Fatalf("app never healthy must not alert (covered by DeployFailed): %v", em.down)
	}
}

func TestWatcherGateSkipsWhenNoHealthChannels(t *testing.T) {
	name := docker.ServiceName(5)
	eng := &scriptEngine{states: map[string]docker.ServiceState{name: downState()}}
	st := &fakeStore{health: 0, watched: []WatchedApp{{AppID: 5, OrgID: 7, Name: "web"}}}
	em := &fakeEmitter{}
	w := newTestWatcher(eng, st, em)
	for i := 0; i < 5; i++ {
		w.tick(context.Background())
	}
	if len(em.down) != 0 || len(em.recovered) != 0 {
		t.Fatal("gate should skip polling entirely when no health channels")
	}
}

func TestWatcherPrunesDeletedApps(t *testing.T) {
	n5, n6 := docker.ServiceName(5), docker.ServiceName(6)
	eng := &scriptEngine{states: map[string]docker.ServiceState{n5: healthyState(), n6: healthyState()}}
	st := &fakeStore{health: 1, watched: []WatchedApp{{AppID: 5, Name: "a"}, {AppID: 6, Name: "b"}}}
	w := newTestWatcher(eng, st, &fakeEmitter{})
	ctx := context.Background()
	w.tick(ctx)
	if len(w.state) != 2 {
		t.Fatalf("want 2 state entries, got %d", len(w.state))
	}
	// App 6 deleted → drops out of the watched set; its state entry must be pruned.
	st.watched = []WatchedApp{{AppID: 5, Name: "a"}}
	w.tick(ctx)
	if len(w.state) != 1 {
		t.Fatalf("want 1 state entry after prune, got %d", len(w.state))
	}
	if _, ok := w.state[6]; ok {
		t.Fatal("deleted app 6 should be pruned from state")
	}
}

func TestWatcherInactiveNotAlerted(t *testing.T) {
	name := docker.ServiceName(5)
	eng := &scriptEngine{states: map[string]docker.ServiceState{name: healthyState()}}
	st := &fakeStore{health: 1, watched: []WatchedApp{{AppID: 5, OrgID: 7, Name: "web"}}}
	em := &fakeEmitter{}
	w := newTestWatcher(eng, st, em)
	ctx := context.Background()
	w.tick(ctx)                                                                  // healthy baseline
	eng.states[name] = docker.ServiceState{Found: true, Desired: 0, Running: 0} // stopped on purpose
	w.tick(ctx)
	w.tick(ctx)
	if len(em.down) != 0 {
		t.Fatalf("intentionally stopped app must not alert: %v", em.down)
	}
}
