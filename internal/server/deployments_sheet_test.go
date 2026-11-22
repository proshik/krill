package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// scaledDownEngine reports every service scaled to zero, as Swarm does for
// an app stopped through the API while its stored status still reads running.
type scaledDownEngine struct{ noopEngine }

func (scaledDownEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	out := map[string]docker.ServiceState{}
	for _, n := range names {
		out[n] = docker.ServiceState{Found: true, Desired: 0}
	}
	return out, nil
}

type sheetFixture struct {
	h      http.Handler
	q      *db.Queries
	cookie *http.Cookie
	base   string
	app    db.Application
}

func newSheetFixture(t *testing.T, eng docker.Engine, source string) sheetFixture {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	orgSvc := org.NewService(q)
	owner := mkUser(t, q, "sheet-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "Sheet")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	params := db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "sheet-web.example.com", Port: 80,
		SourceType: source, DockerfilePath: "Dockerfile",
	}
	if source == "dockerfile" {
		params.GitUrl, params.GitBranch = "https://git.example.com/shop/web.git", "main"
	}
	a, err := q.CreateApplication(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", AllowPrivateEgress: true}
	hub := deploy.NewLogHub()
	h := server.New(cfg, auth.NewService(q), orgSvc, q, nil, eng, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net")).Router()
	return sheetFixture{h: h, q: q, cookie: loginAs(t, q, "sheet-owner@k.local"), app: a,
		base: "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID)}
}

func (f sheetFixture) deploy(t *testing.T, tag string) db.Deployment {
	t.Helper()
	ctx := context.Background()
	dep, err := f.q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: f.app.ID, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: dep.ID, Status: "done", ImageTag: tag, Log: "log"}); err != nil {
		t.Fatal(err)
	}
	return dep
}

func (f sheetFixture) get(t *testing.T, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, rec.Code)
	}
	return rec.Body.String()
}

// The deployments tab keeps polling after its first refresh (the polling
// element stays, its content is swapped), and the app's action forms live
// outside the polled part, so a refresh cannot drop a confirmed Stop. Of two
// successful releases only the current one is "active"; the older one is
// "removed" in the history.
func TestDeploymentsTabPollingFormsAndHistory(t *testing.T) {
	f := newSheetFixture(t, noopEngine{}, "image")
	if err := f.q.UpdateApplicationStatus(context.Background(), db.UpdateApplicationStatusParams{ID: f.app.ID, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	old := f.deploy(t, "nginx:1.0")
	cur := f.deploy(t, "nginx:1.1")

	page := f.get(t, f.base+"?tab=deployments")
	if !strings.Contains(page, `hx-get="`+f.base+`/deployments-list" hx-trigger="every 3s" hx-swap="innerHTML"`) {
		t.Error("the deployments list must refresh its content, keeping the polling element")
	}
	if strings.Count(page, `id="stop-form"`) != 1 {
		t.Error("the stop form must be on the page once")
	}

	frag := f.get(t, f.base+"/deployments-list")
	if strings.Contains(frag, `id="stop-form"`) || strings.Contains(frag, `id="deploy-again"`) {
		t.Error("a refresh must not carry the action forms: it would replace one whose confirmation is open")
	}
	if !strings.Contains(frag, `/deployments/`+i64(cur.ID)+`"`) || !strings.Contains(frag, `/deployments/`+i64(old.ID)+`"`) {
		t.Fatal("the fragment lacks the deployments' own rows")
	}
	if n := strings.Count(frag, `tag--active`); n != 1 {
		t.Errorf("want one active release, got %d", n)
	}
	if !strings.Contains(frag, `tag--removed">removed<`) {
		t.Error("a replaced release must be labelled removed in the history")
	}
}

// The refresh shows the same live status as the sheet: an app Swarm reports
// scaled to zero offers Start, not Stop, even while its stored status still
// reads running.
func TestDeploymentsRefreshUsesLiveStatus(t *testing.T) {
	f := newSheetFixture(t, scaledDownEngine{}, "image")
	if err := f.q.UpdateApplicationStatus(context.Background(), db.UpdateApplicationStatusParams{ID: f.app.ID, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	f.deploy(t, "nginx:1.0")
	frag := f.get(t, f.base+"/deployments-list")
	if strings.Contains(frag, `form="stop-form"`) || !strings.Contains(frag, `>start<`) {
		t.Fatal("the refresh used the stored running status of a stopped app")
	}
}

// An idle Dockerfile app can be rebuilt without cache before it starts, as
// before the redesign.
func TestIdleDockerfileAppOffersRebuild(t *testing.T) {
	f := newSheetFixture(t, noopEngine{}, "dockerfile")
	page := f.get(t, f.base+"?tab=deployments")
	if !strings.Contains(page, `form="rebuild-form"`) || !strings.Contains(page, `form="deploy-again"`) {
		t.Fatal("an idle Dockerfile app must offer Start and Rebuild")
	}
}
