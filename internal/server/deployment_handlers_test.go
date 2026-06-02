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
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

func TestDeploymentsListShowsRows(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	// owner + org + проект + окружение + приложение + деплой
	hash, _ := auth.HashPassword("pw")
	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "o@k", PasswordHash: hash})
	orgSvc := org.NewService(q)
	o, _ := orgSvc.CreateOrg(ctx, u.ID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "web.x", Port: 80,
		Env: map[string]string{}, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	dep, _ := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: a.ID, Trigger: "manual"})
	_ = q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: dep.ID, Status: "done", ImageTag: "nginx:alpine", Log: "hello build log"})

	authSvc := auth.NewService(q)
	tok, _ := authSvc.Authenticate(ctx, "o@k", "pw")
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	h := server.New(cfg, authSvc, orgSvc, q, nil, nil, deploy.NewLogHub(), dbservice.New(nil, dbservice.NewDBStore(q), deploy.NewLogHub(), "krill-net")).Router()

	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID)

	// список деплоев
	req := httptest.NewRequest(http.MethodGet, base+"?tab=deployments", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deployments tab want 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "nginx:alpine") {
		t.Errorf("deployments table missing image tag; body:\n%s", body)
	}
}
