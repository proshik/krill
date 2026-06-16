package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestAddDBLinkHappyPath(t *testing.T) {
	h, q, orgSvc := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "dbl-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "web.x", Port: 80,
		EnvText: "FOO=bar", SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	pg, _ := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID: e.ID, Name: "maindb", AppName: "krill-postgres-maindb",
		DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: "pw", Image: "postgres:17",
	})
	cookie := loginAs(t, q, "dbl-owner@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)

	rec := postForm(t, h, base+"/db-links", cookie, url.Values{
		"db_ref": {"postgres:" + i64(pg.ID)}, "var_name": {"DATABASE_URL"}, "scheme": {"postgres"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("addDBLink: got %d, want 303", rec.Code)
	}
	links, _ := q.ListDBLinksByApplication(ctx, app.ID)
	if len(links) != 1 || links[0].VarName != "DATABASE_URL" || links[0].Scheme != "postgres" {
		t.Fatalf("link not persisted: %+v", links)
	}
	// redis happy path (scheme forced to "redis" server-side)
	rd, _ := q.CreateRedis(ctx, db.CreateRedisParams{
		EnvironmentID: e.ID, Name: "cache", AppName: "krill-redis-cache", Password: "rpw", Image: "redis:7-alpine",
	})
	if rec := postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"redis:" + i64(rd.ID)}, "var_name": {"REDIS_URL"}, "scheme": {"redis"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("addDBLink redis: got %d, want 303", rec.Code)
	}
	// reject: invalid scheme for postgres
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"postgres:" + i64(pg.ID)}, "var_name": {"OTHER_URL"}, "scheme": {"mysql"}})
	// reject: var already in env_text
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"postgres:" + i64(pg.ID)}, "var_name": {"FOO"}, "scheme": {"postgres"}})
	// reject: invalid var name
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"postgres:" + i64(pg.ID)}, "var_name": {"bad name"}, "scheme": {"postgres"}})
	// reject: duplicate var name
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"postgres:" + i64(pg.ID)}, "var_name": {"DATABASE_URL"}, "scheme": {"postgres"}})
	// only the two valid links (DATABASE_URL + REDIS_URL) persist
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 2 {
		t.Fatalf("expected 2 persisted links, have %d", len(links))
	}
}

func TestDBLinkCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc := newDeployServer(t)
	ctx := context.Background()
	uidA := mkUser(t, q, "dbl-a@k.local")
	oA, _ := orgSvc.CreateOrg(ctx, uidA, "OrgA")
	pA, _ := orgSvc.CreateProject(ctx, oA.ID, "PA", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "prod-a")
	appA, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eA.ID, Name: "a", Image: "nginx", Tag: "alpine", Domain: "a.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	pgA, _ := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID: eA.ID, Name: "dba", AppName: "krill-postgres-dba",
		DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: "pw", Image: "postgres:17",
	})
	linkA, _ := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: appA.ID, Engine: "postgres", DbID: pgA.ID, VarName: "DATABASE_URL", Scheme: "postgres"})

	uidB := mkUser(t, q, "dbl-b@k.local")
	oB, _ := orgSvc.CreateOrg(ctx, uidB, "OrgB")
	pB, _ := orgSvc.CreateProject(ctx, oB.ID, "PB", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "prod-b")
	appB, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eB.ID, Name: "b", Image: "nginx", Tag: "alpine", Domain: "b.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	cookieB := loginAs(t, q, "dbl-b@k.local")
	target := "/orgs/" + i64(oB.ID) + "/projects/" + i64(pB.ID) + "/environments/" + i64(eB.ID) +
		"/apps/" + i64(appB.ID) + "/db-links/" + i64(linkA.ID) + "/delete"
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookieB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("SECURITY: cross-tenant db-link delete got %d, want 404", rec.Code)
	}
	if _, err := q.GetDBLink(ctx, linkA.ID); err != nil {
		t.Fatalf("SECURITY: org-A link affected by cross-tenant request: %v", err)
	}
}
