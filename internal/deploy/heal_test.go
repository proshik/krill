package deploy

import (
	"context"
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

func (f *fakeStore) HealAppStatus(_ context.Context, app int64, status string) (bool, error) {
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
	recent, old := now.Add(-time.Minute), now.Add(-settleTimeout)
	cases := []struct {
		name string
		st   docker.ServiceState
		last time.Time
		want string
	}{
		{"no service", docker.ServiceState{}, recent, StatusIdle},
		{"scaled to zero", docker.ServiceState{Found: true}, recent, StatusIdle},
		{"converged", docker.ServiceState{Found: true, Running: 1, Desired: 1}, old, StatusRunning},
		{"still starting", docker.ServiceState{Found: true, Running: 0, Desired: 1}, recent, ""},
		{"never converged", docker.ServiceState{Found: true, Running: 1, Desired: 2}, old, StatusError},
	}
	for _, c := range cases {
		if got := healedStatus(c.st, c.last, now); got != c.want {
			t.Errorf("%s: healedStatus = %q, want %q", c.name, got, c.want)
		}
	}
}

type statesEngine map[string]docker.ServiceState

func (e statesEngine) ServiceStates(context.Context, []string) (map[string]docker.ServiceState, error) {
	return e, nil
}

// The queries against a real schema: msk's state (deploy 36 done, app left
// "deploying", service 1/1) heals to running; an app with a deployment still
// running is never listed; and a deploy that starts between the listing and
// the write keeps its "deploying".
func TestHealStuckStatusesOnDB(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "heal@k.local", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-heal", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p-heal", Description: ""})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "prod", Slug: "prod"})
	newApp := func(name string) int64 {
		t.Helper()
		a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
			EnvironmentID: e.ID, Name: name, Image: "nginx", Tag: "alpine",
			Domain: name + ".heal", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
		})
		if err != nil {
			t.Fatalf("create app: %v", err)
		}
		if err := q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: a.ID, Status: StatusDeploying}); err != nil {
			t.Fatalf("set status: %v", err)
		}
		return a.ID
	}
	deployment := func(app int64, finish string) {
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

	s := NewDBStore(q)
	listed, err := s.ListStuckDeployingApps(ctx)
	if err != nil {
		t.Fatalf("ListStuckDeployingApps: %v", err)
	}
	ids := map[int64]bool{}
	for _, a := range listed {
		ids[a.ID] = true
		if a.LastFinished.IsZero() {
			t.Errorf("app %d listed without the time its last deployment ended", a.ID)
		}
	}
	if !ids[stuck] || !ids[raced] || ids[inFlight] || len(ids) != 2 {
		t.Fatalf("listed %v, want exactly %d and %d", ids, stuck, raced)
	}

	// A new deploy of "raced" starts after the listing, before the write.
	deployment(raced, "")
	up := docker.ServiceState{Found: true, Running: 1, Desired: 1}
	eng := statesEngine{docker.ServiceName(stuck): up, docker.ServiceName(raced): up, docker.ServiceName(inFlight): up}
	if ok, err := s.HealAppStatus(ctx, raced, StatusRunning); err != nil || ok {
		t.Fatalf("HealAppStatus on an app with a deploy running = %v, %v; want false", ok, err)
	}
	n, err := HealStuckStatuses(ctx, s, eng, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("HealStuckStatuses = %d, %v; want 1", n, err)
	}
	if got := status(stuck); got != StatusRunning {
		t.Errorf("stuck app status = %q, want running", got)
	}
	if got := status(raced); got != StatusDeploying {
		t.Errorf("app whose deploy started meanwhile = %q, want deploying", got)
	}
	if got := status(inFlight); got != StatusDeploying {
		t.Errorf("app with a deploy in flight = %q, want deploying", got)
	}
}
