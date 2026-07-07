package server_test

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// recordingEngine is a noopEngine that records VolumeRemove calls.
type recordingEngine struct {
	noopEngine
	mu      sync.Mutex
	removed []string
}

func (e *recordingEngine) VolumeRemove(_ context.Context, name string) error {
	e.mu.Lock()
	e.removed = append(e.removed, name)
	e.mu.Unlock()
	return nil
}

func (e *recordingEngine) removedNames() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.removed...)
}

// newServerWithEngine mirrors newDeployServer but injects a custom engine.
func newServerWithEngine(t *testing.T, eng *recordingEngine) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", AllowPrivateEgress: true}
	hub := deploy.NewLogHub()
	dep := deploy.New(eng, noopBuilder{}, deploy.NewDBStore(q), hub, "krill-net")
	dep.Start(context.Background())
	t.Cleanup(dep.Stop)
	dbSvc := dbservice.New(eng, dbservice.NewDBStore(q), hub, "krill-net")
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, dep, eng, hub, dbSvc)
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q), true), func() {})
	return srv.Router(), q, orgSvc
}

func TestDeleteAppDestroyDataRemovesVolumes(t *testing.T) {
	eng := &recordingEngine{}
	h, q, orgSvc := newServerWithEngine(t, eng)
	ctx := context.Background()
	uid := mkUser(t, q, "del-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	_, _ = q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"})
	cookie := loginAs(t, q, "del-owner@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) +
		"/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)

	// destroy_data=on → the Docker volume is removed.
	if rec := postForm(t, h, base+"/delete", cookie, url.Values{"destroy_data": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deleteApp: got %d, want 303", rec.Code)
	}
	got := eng.removedNames()
	if len(got) != 1 || got[0] != "krill-vol-"+i64(app.ID)+"-data" {
		t.Fatalf("expected VolumeRemove(krill-vol-%s-data), got %v", i64(app.ID), got)
	}
}

func TestDeleteAppWithoutDestroyDataKeepsVolumes(t *testing.T) {
	eng := &recordingEngine{}
	h, q, orgSvc := newServerWithEngine(t, eng)
	ctx := context.Background()
	uid := mkUser(t, q, "keep-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	_, _ = q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"})
	cookie := loginAs(t, q, "keep-owner@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) +
		"/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)

	if rec := postForm(t, h, base+"/delete", cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deleteApp: got %d, want 303", rec.Code)
	}
	if got := eng.removedNames(); len(got) != 0 {
		t.Fatalf("no destroy_data → no VolumeRemove, got %v", got)
	}
}

// Deleting an environment always removes its apps' Docker volumes (like managed
// DBs) — there is no UI left to clean them otherwise.
func TestDeleteEnvironmentRemovesAppVolumes(t *testing.T) {
	eng := &recordingEngine{}
	h, q, orgSvc := newServerWithEngine(t, eng)
	ctx := context.Background()
	uid := mkUser(t, q, "env-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	_, _ = q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"})
	cookie := loginAs(t, q, "env-owner@k.local")

	envDelete := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/delete"
	if rec := postForm(t, h, envDelete, cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("deleteEnvironment: got %d, want 303", rec.Code)
	}
	got := eng.removedNames()
	if len(got) != 1 || got[0] != "krill-vol-"+i64(app.ID)+"-data" {
		t.Fatalf("expected env delete to remove krill-vol-%s-data, got %v", i64(app.ID), got)
	}
}
