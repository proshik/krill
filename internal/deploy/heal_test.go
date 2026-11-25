package deploy

import (
	"context"
	"errors"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/testutil"
)

// The fake store's side of StatusHealer: a deployment is running until
// FinishDeployment records it.
func (f *fakeStore) ListStuckDeployingApps(context.Context) ([]StuckApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StuckApp
	for app, st := range f.status {
		if st == StatusDeploying && !f.runningFor(app) {
			out = append(out, StuckApp{ID: app})
		}
	}
	return out, nil
}

func (f *fakeStore) HealAppStatus(_ context.Context, app int64, status string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status[app] != StatusDeploying || f.runningFor(app) {
		return false, nil
	}
	f.status[app] = status
	return true, nil
}

func (f *fakeStore) runningFor(app int64) bool {
	for dep, a := range f.depApp {
		if _, finished := f.deploys[dep]; a == app && !finished {
			return true
		}
	}
	return false
}

// Krill stopping while a deploy settles records the deployment interrupted and
// leaves the app "deploying" — and nothing used to rewrite that status, so the
// app read "deploying" forever (base13 on msk). After the restart the heal
// sets it from the service, which has converged meanwhile.
func TestInterruptedDeployStatusHealsAfterRestart(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	id := d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond)
	d.Stop()
	if got := st.depStatus(id); got != "error" {
		t.Fatalf("precondition: deployment status = %q, want error", got)
	}
	if got := st.appStatus(1); got != StatusDeploying {
		t.Fatalf("precondition: app status = %q, want deploying", got)
	}

	eng.setPartial(false) // Swarm finished the rollout without Krill
	n, err := HealStuckStatuses(context.Background(), st, eng, time.Now())
	if err != nil {
		t.Fatalf("HealStuckStatuses: %v", err)
	}
	if n != 1 || st.appStatus(1) != StatusRunning {
		t.Fatalf("healed %d, app status = %q; want 1 and running", n, st.appStatus(1))
	}
}

// A deploy in flight owns the app's status: the heal leaves it alone.
func TestHealLeavesAppWithRunningDeploy(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()
	d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond) // past convergeTimeout: settling
	eng.setPartial(false)
	if n, err := HealStuckStatuses(context.Background(), st, eng, time.Now()); err != nil || n != 0 {
		t.Fatalf("HealStuckStatuses = %d, %v; want 0, nil", n, err)
	}
}

func TestHealedStatus(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	recent := StuckApp{LastFinished: now.Add(-time.Minute), LastStatus: "error"}
	old := StuckApp{LastFinished: now.Add(-settleTimeout), LastStatus: "error"}
	done := StuckApp{LastFinished: now.Add(-time.Minute), LastStatus: "done"}
	cases := []struct {
		name string
		p    docker.ServiceProgress
		last StuckApp
		want string
	}{
		{"no service after a done deploy (stopped global app)", docker.ServiceProgress{}, done, StatusIdle},
		{"no service after a failed deploy (interrupted first deploy)", docker.ServiceProgress{}, recent, StatusError},
		{"no service after a shutdown interrupted the deploy", docker.ServiceProgress{}, StuckApp{LastFinished: now, LastStatus: "error", LastError: "interrupted: krill stopped before the service converged"}, StatusError},
		{"no service after a stop cancelled the deploy (global app)", docker.ServiceProgress{}, StuckApp{LastFinished: now, LastStatus: "error", LastError: "stopped by admin@k.local before it was applied"}, StatusIdle},
		{"no service after a stop during settle (v0.13.1 API stop)", docker.ServiceProgress{}, StuckApp{LastFinished: now, LastStatus: "error", LastError: "stopped before the service converged"}, StatusIdle},
		{"scaled to zero", docker.ServiceProgress{Found: true}, recent, StatusIdle},
		{"converged", docker.ServiceProgress{Found: true, Running: 1, Desired: 1, UpdateState: "completed"}, old, StatusRunning},
		{"never updated", docker.ServiceProgress{Found: true, Running: 1, Desired: 1}, old, StatusRunning},
		// The old replica of a start-first update still runs: not converged.
		{"update in progress", docker.ServiceProgress{Found: true, Running: 1, Desired: 1, UpdateState: "updating"}, recent, ""},
		{"update stuck past the settle window", docker.ServiceProgress{Found: true, Running: 1, Desired: 1, UpdateState: "updating"}, old, StatusError},
		{"rolling back", docker.ServiceProgress{Found: true, Running: 1, Desired: 1, UpdateState: "rollback_started"}, recent, ""},
		{"rolled back", docker.ServiceProgress{Found: true, Running: 1, Desired: 1, UpdateState: "rollback_completed"}, recent, StatusError},
		{"global, no eligible node yet", docker.ServiceProgress{Found: true, Global: true}, recent, ""},
		{"global, no eligible node past the window", docker.ServiceProgress{Found: true, Global: true}, old, StatusError},
		{"still starting", docker.ServiceProgress{Found: true, Running: 0, Desired: 1}, recent, ""},
		{"never converged", docker.ServiceProgress{Found: true, Running: 1, Desired: 2}, old, StatusError},
	}
	for _, c := range cases {
		if got := healedStatus(c.p, c.last, now); got != c.want {
			t.Errorf("%s: healedStatus = %q, want %q", c.name, got, c.want)
		}
	}
}

type progressEngine map[string]docker.ServiceProgress

func (e progressEngine) ServiceProgress(_ context.Context, name string, _ []string) (docker.ServiceProgress, error) {
	if p, ok := e[name]; ok && p.UpdateState == "lookup-fails" {
		return docker.ServiceProgress{}, errors.New("service removed meanwhile")
	}
	return e[name], nil
}

// scriptedHealer lists fixed apps and records the writes.
type scriptedHealer struct {
	apps   []StuckApp
	writes map[int64]string
}

func (h *scriptedHealer) ListStuckDeployingApps(context.Context) ([]StuckApp, error) {
	return h.apps, nil
}

func (h *scriptedHealer) HealAppStatus(_ context.Context, app int64, status string, _ time.Time) (bool, error) {
	h.writes[app] = status
	return true, nil
}

// One app whose service lookup fails (removed between Docker's two requests)
// is left for the next pass; the apps after it are still healed.
func TestHealContinuesPastAFailedLookup(t *testing.T) {
	up := docker.ServiceProgress{Found: true, Running: 1, Desired: 1}
	eng := progressEngine{docker.ServiceName(1): {UpdateState: "lookup-fails"}, docker.ServiceName(2): up}
	h := &scriptedHealer{apps: []StuckApp{{ID: 1}, {ID: 2}}, writes: map[int64]string{}}
	n, err := HealStuckStatuses(context.Background(), h, eng, time.Now())
	if err == nil {
		t.Error("the failed lookup must still be reported")
	}
	if n != 1 || h.writes[2] != StatusRunning {
		t.Fatalf("healed %d, writes %v; want app 2 running", n, h.writes)
	}
	if _, wrote := h.writes[1]; wrote {
		t.Fatal("app 1 must be left for the next pass")
	}
}

// The queries against a real schema: msk's state (deploy 36 done, app left
// "deploying", service 1/1) heals to running; an app with a deployment still
// running is never listed; and a deploy that starts — or starts and ends —
// between the listing and the write keeps what it wrote.
func TestHealStuckStatusesOnDB(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "heal@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-heal", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p-heal", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "prod", Slug: "prod"})
	setStatus := func(app int64, st string) {
		t.Helper()
		if err := q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: app, Status: st}); err != nil {
			t.Fatalf("set status: %v", err)
		}
	}
	newApp := func(name string) int64 {
		t.Helper()
		a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
			EnvironmentID: e.ID, Name: name, Image: "nginx", Tag: "alpine",
			Domain: name + ".heal", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
		})
		if err != nil {
			t.Fatalf("create app: %v", err)
		}
		setStatus(a.ID, StatusDeploying)
		return a.ID
	}
	deployment := func(app int64, finish string) int64 {
		t.Helper()
		d, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: app, Trigger: "api"})
		if err != nil {
			t.Fatalf("create deployment: %v", err)
		}
		if finish != "" {
			if err := q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: d.ID, Status: finish}); err != nil {
				t.Fatalf("finish deployment: %v", err)
			}
		}
		return d.ID
	}
	status := func(app int64) string {
		t.Helper()
		a, err := q.GetApplication(ctx, app)
		if err != nil {
			t.Fatalf("get app: %v", err)
		}
		return a.Status
	}

	stuck := newApp("stuck")
	deployment(stuck, "done")
	inFlight := newApp("inflight")
	deployment(inFlight, "")
	raced := newApp("raced")
	deployment(raced, "error")
	ended := newApp("ended")
	deployment(ended, "error")

	s := NewDBStore(q)
	listed, err := s.ListStuckDeployingApps(ctx)
	if err != nil {
		t.Fatalf("ListStuckDeployingApps: %v", err)
	}
	byID := map[int64]StuckApp{}
	for _, a := range listed {
		byID[a.ID] = a
		if a.LastFinished.IsZero() || a.Seen.IsZero() {
			t.Errorf("app %d listed without the time its last deployment ended or its row changed", a.ID)
		}
	}
	if _, ok := byID[inFlight]; ok || len(byID) != 3 {
		t.Fatalf("listed %v, want stuck, raced and ended only", byID)
	}
	if byID[stuck].LastStatus != "done" || byID[raced].LastStatus != "error" {
		t.Errorf("last deployment status: stuck %q, raced %q", byID[stuck].LastStatus, byID[raced].LastStatus)
	}

	// After the listing: a deploy of "raced" starts (submit writes the row,
	// then the status); one of "ended" starts and is interrupted at shutdown,
	// which leaves the app "deploying" again with no deploy running.
	deployment(raced, "")
	setStatus(raced, StatusDeploying)
	deployment(ended, "error")
	setStatus(ended, StatusDeploying)
	if ok, err := s.HealAppStatus(ctx, raced, StatusRunning, byID[raced].Seen); err != nil || ok {
		t.Fatalf("HealAppStatus on an app with a deploy running = %v, %v; want false", ok, err)
	}
	if ok, err := s.HealAppStatus(ctx, ended, StatusIdle, byID[ended].Seen); err != nil || ok {
		t.Fatalf("HealAppStatus from a listing older than the last deploy = %v, %v; want false", ok, err)
	}
	if ok, err := s.HealAppStatus(ctx, stuck, StatusRunning, byID[stuck].Seen); err != nil || !ok {
		t.Fatalf("HealAppStatus on the stuck app = %v, %v; want true", ok, err)
	}
	if got := status(stuck); got != StatusRunning {
		t.Errorf("stuck app status = %q, want running", got)
	}
	for _, app := range []int64{raced, ended, inFlight} {
		if got := status(app); got != StatusDeploying {
			t.Errorf("app %d status = %q, want deploying", app, got)
		}
	}

	// A full pass from a fresh listing: "ended" heals from its live service,
	// "raced" (deploy running) and "inflight" stay.
	up := docker.ServiceProgress{Found: true, Running: 1, Desired: 1, UpdateState: "completed"}
	eng := progressEngine{docker.ServiceName(ended): up, docker.ServiceName(raced): up, docker.ServiceName(inFlight): up}
	if n, err := HealStuckStatuses(ctx, s, eng, time.Now()); err != nil || n != 1 {
		t.Fatalf("HealStuckStatuses = %d, %v; want 1", n, err)
	}
	if got := status(ended); got != StatusRunning {
		t.Errorf("ended app status = %q, want running", got)
	}
}

// A deploy writing in a transaction that overlaps the heal's UPDATE: the
// UPDATE waits for the application row, then sees the newer row version —
// its updated_at no longer matches the listing, so nothing is written even
// though the statement's snapshot of deployments predates the new row.
func TestHealWaitsForAnOverlappingDeploy(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "overlap@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-overlap", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p-overlap", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "prod", Slug: "prod"})
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.overlap", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: a.ID, Status: StatusDeploying}); err != nil {
		t.Fatal(err)
	}
	s := NewDBStore(q)
	listed, err := s.ListStuckDeployingApps(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed %v, %v", listed, err)
	}

	// The writer: a new deploy's row and its "deploying", not yet committed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	qtx := q.WithTx(tx)
	if _, err := qtx.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: a.ID, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	if err := qtx.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: a.ID, Status: StatusDeploying}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := s.HealAppStatus(ctx, a.ID, StatusIdle, listed[0].Seen)
		done <- result{ok, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the heal did not wait for the writer's row lock: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.ok {
		t.Fatalf("HealAppStatus over an overlapping deploy = %v, %v; want false", r.ok, r.err)
	}
	got, _ := q.GetApplication(ctx, a.ID)
	if got.Status != StatusDeploying {
		t.Fatalf("status = %q, want deploying", got.Status)
	}
}
