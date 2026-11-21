package notify

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

type fakeEmitter struct {
	down      []int64
	recovered []int64

	dbDown      []int64
	dbDetail    []string
	dbRecovered []int64
}

func (f *fakeEmitter) DBDown(_ context.Context, id int64, detail string) {
	f.dbDown = append(f.dbDown, id)
	f.dbDetail = append(f.dbDetail, detail)
}
func (f *fakeEmitter) DBRecovered(_ context.Context, id int64) {
	f.dbRecovered = append(f.dbRecovered, id)
}

func (f *fakeEmitter) AppDown(_ context.Context, appID int64, _ string) {
	f.down = append(f.down, appID)
}
func (f *fakeEmitter) AppRecovered(_ context.Context, appID int64) {
	f.recovered = append(f.recovered, appID)
}

type scriptEngine struct {
	states map[string]docker.ServiceState
}

func (s *scriptEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	out := map[string]docker.ServiceState{}
	for _, n := range names {
		out[n] = s.states[n]
	}
	return out, nil
}

func healthyState() docker.ServiceState {
	return docker.ServiceState{Found: true, Desired: 1, Running: 1}
}
func downState() docker.ServiceState { return docker.ServiceState{Found: true, Desired: 1, Running: 0} }

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
	if _, ok := w.state[watchKey{id: 6}]; ok {
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
	w.tick(ctx)                                                                 // healthy baseline
	eng.states[name] = docker.ServiceState{Found: true, Desired: 0, Running: 0} // stopped on purpose
	w.tick(ctx)
	w.tick(ctx)
	if len(em.down) != 0 {
		t.Fatalf("intentionally stopped app must not alert: %v", em.down)
	}
}

// A database server whose node is powered off: Swarm keeps desired=1 and
// queues a replacement task that cannot be placed (the service is pinned), so
// nothing runs. That must alert once and recover once — before this, only
// apps were watched and a four-hour database outage went unreported.
func TestWatcherDatabaseServerDownAndRecovered(t *testing.T) {
	const svc = "krill-postgres-main-abc123"
	eng := &scriptEngine{states: map[string]docker.ServiceState{svc: healthyState()}}
	st := &fakeStore{health: 1, watchedDBs: []WatchedDB{{InstanceID: 1, OrgID: 7, Name: "main", Service: svc, Node: "node-1", Status: "running"}}}
	em := &fakeEmitter{}
	w := newTestWatcher(eng, st, em)
	ctx := context.Background()

	w.tick(ctx)
	eng.states[svc] = downState()
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	if len(em.dbDown) != 1 || em.dbDown[0] != 1 {
		t.Fatalf("expected one DBDown for instance 1, got %v", em.dbDown)
	}
	if em.dbDetail[0] != "0/1 tasks running · node-1" {
		t.Fatalf("detail = %q", em.dbDetail[0])
	}
	if len(em.down) != 0 {
		t.Fatalf("a database server must not be reported as an app: %v", em.down)
	}
	eng.states[svc] = healthyState()
	w.tick(ctx)
	if len(em.dbRecovered) != 1 || em.dbRecovered[0] != 1 {
		t.Fatalf("expected one DBRecovered, got %v", em.dbRecovered)
	}
}

// App 1 and database server 1 are different services: their health states
// must not share an entry.
func TestWatcherAppAndDatabaseWithSameIDAreIndependent(t *testing.T) {
	const svc = "krill-redis-cache-def456"
	app := docker.ServiceName(1)
	eng := &scriptEngine{states: map[string]docker.ServiceState{app: healthyState(), svc: healthyState()}}
	st := &fakeStore{health: 1,
		watched:    []WatchedApp{{AppID: 1, OrgID: 7, Name: "web"}},
		watchedDBs: []WatchedDB{{InstanceID: 1, OrgID: 7, Name: "cache", Service: svc, Status: "running"}}}
	em := &fakeEmitter{}
	w := newTestWatcher(eng, st, em)
	ctx := context.Background()
	w.tick(ctx)
	eng.states[svc] = downState()
	w.tick(ctx)
	w.tick(ctx)
	if len(em.dbDown) != 1 || len(em.down) != 0 {
		t.Fatalf("dbDown=%v appDown=%v", em.dbDown, em.down)
	}
}

// Stopping a server (desired 0) and moving it to another node are intentional:
// neither alerts.
func TestWatcherDatabaseServerIntentionalStopsAreSilent(t *testing.T) {
	const svc = "krill-postgres-main-abc123"
	eng := &scriptEngine{states: map[string]docker.ServiceState{svc: healthyState()}}
	st := &fakeStore{health: 1, watchedDBs: []WatchedDB{{InstanceID: 1, OrgID: 7, Name: "main", Service: svc, Status: "running"}}}
	em := &fakeEmitter{}
	w := newTestWatcher(eng, st, em)
	ctx := context.Background()
	w.tick(ctx)

	eng.states[svc] = docker.ServiceState{Found: true, Desired: 0}
	w.tick(ctx)
	w.tick(ctx)

	eng.states[svc] = downState()
	st.watchedDBs[0].Status = "migrating"
	w.tick(ctx)
	w.tick(ctx)
	w.tick(ctx)
	if len(em.dbDown) != 0 || len(em.dbRecovered) != 0 {
		t.Fatalf("intentional stop alerted: down=%v recovered=%v", em.dbDown, em.dbRecovered)
	}
}
