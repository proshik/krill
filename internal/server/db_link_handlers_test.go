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
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "dbl-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "web.x", Port: 80,
		EnvText: "FOO=bar", SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	inst, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg1", AppName: "krill-postgres-pg1-t1",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	ldb, _ := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "maindb", DbName: "maindb", Username: "maindb", Password: "pw",
	})
	cookie := loginAs(t, q, "dbl-owner@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)

	rec := postForm(t, h, base+"/db-links", cookie, url.Values{
		"db_ref": {"pg:" + i64(ldb.ID)}, "var_name": {"DATABASE_URL"}, "scheme": {"postgres"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("addDBLink: got %d, want 303", rec.Code)
	}
	links, _ := q.ListDBLinksByApplication(ctx, app.ID)
	if len(links) != 1 || links[0].VarName != "DATABASE_URL" || links[0].Scheme != "postgres" {
		t.Fatalf("link not persisted: %+v", links)
	}
	if links[0].LogicalDatabaseID == nil || *links[0].LogicalDatabaseID != ldb.ID || links[0].InstanceID != nil {
		t.Fatalf("link FK mismatch: %+v", links[0])
	}
	// redis happy path (scheme forced to "redis" server-side)
	redisInst, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "redis", Name: "cache", AppName: "krill-redis-cache-t1",
		Image: "redis:7-alpine", SuperuserPassword: "rpw",
	})
	if rec := postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"redis:" + i64(redisInst.ID)}, "var_name": {"REDIS_URL"}, "scheme": {"redis"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("addDBLink redis: got %d, want 303", rec.Code)
	}
	// reject: invalid scheme for postgres
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"pg:" + i64(ldb.ID)}, "var_name": {"OTHER_URL"}, "scheme": {"mysql"}})
	// reject: var already in env_text
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"pg:" + i64(ldb.ID)}, "var_name": {"FOO"}, "scheme": {"postgres"}})
	// reject: invalid var name
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"pg:" + i64(ldb.ID)}, "var_name": {"bad name"}, "scheme": {"postgres"}})
	// reject: duplicate var name
	postForm(t, h, base+"/db-links", cookie, url.Values{"db_ref": {"pg:" + i64(ldb.ID)}, "var_name": {"DATABASE_URL"}, "scheme": {"postgres"}})
	// only the two valid links (DATABASE_URL + REDIS_URL) persist
	if links, _ := q.ListDBLinksByApplication(ctx, app.ID); len(links) != 2 {
		t.Fatalf("expected 2 persisted links, have %d", len(links))
	}
}

func TestDBLinkCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uidA := mkUser(t, q, "dbl-a@k.local")
	oA, _ := orgSvc.CreateOrg(ctx, uidA, "OrgA")
	pA, _ := orgSvc.CreateProject(ctx, oA.ID, "PA", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "prod-a")
	appA, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eA.ID, Name: "a", Image: "nginx", Tag: "alpine", Domain: "a.x", Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	instA, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: oA.ID, Engine: "postgres", Name: "dba", AppName: "krill-postgres-dba-t2",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	ldbA, _ := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: instA.ID, EnvironmentID: eA.ID, Name: "dba", DbName: "app", Username: "postgres", Password: "pw",
	})
	linkA, _ := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: appA.ID, LogicalDatabaseID: &ldbA.ID, VarName: "DATABASE_URL", Scheme: "postgres", Field: "url"})
	redisInstA, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: oA.ID, Engine: "redis", Name: "cache-a", AppName: "krill-redis-cache-a-t2",
		Image: "redis:7-alpine", SuperuserPassword: "rpw",
	})

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

	// cross-env confinement: org-B must not be able to link org-A's logical DB
	// (which lives in another environment) to its own app, even by supplying
	// the raw id.
	baseB := "/orgs/" + i64(oB.ID) + "/projects/" + i64(pB.ID) + "/environments/" + i64(eB.ID) + "/apps/" + i64(appB.ID)
	postForm(t, h, baseB+"/db-links", cookieB, url.Values{
		"db_ref": {"pg:" + i64(ldbA.ID)}, "var_name": {"X_URL"}, "scheme": {"postgres"},
	})
	if links, _ := q.ListDBLinksByApplication(ctx, appB.ID); len(links) != 0 {
		t.Fatalf("SECURITY: org-B linked org-A's logical DB by id, have %d links", len(links))
	}

	// cross-org confinement: org-B must not be able to link org-A's redis
	// instance either, even by supplying the raw id.
	postForm(t, h, baseB+"/db-links", cookieB, url.Values{
		"db_ref": {"redis:" + i64(redisInstA.ID)}, "var_name": {"Y_URL"}, "scheme": {"redis"},
	})
	if links, _ := q.ListDBLinksByApplication(ctx, appB.ID); len(links) != 0 {
		t.Fatalf("SECURITY: org-B linked org-A's redis instance by id, have %d links", len(links))
	}
}

func TestAddDBLinkPerField(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "dblf@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "wf.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	inst, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg", AppName: "krill-postgres-x",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	ld, _ := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "app", DbName: "app", Username: "app", Password: "pw",
	})
	redis, _ := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "redis", Name: "rd", AppName: "krill-redis-x",
		Image: "redis:7", Superuser: "default", SuperuserPassword: "pw",
	})
	cookie := loginAs(t, q, "dblf@k.local")
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)

	// per-field postgres password link persists field=password
	if rec := postForm(t, h, base+"/db-links", cookie, url.Values{
		"db_ref": {"pg:" + i64(ld.ID)}, "var_name": {"DB_PASSWORD"}, "scheme": {"postgres"}, "field": {"password"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add password link: got %d", rec.Code)
	}
	links, _ := q.ListDBLinksByApplication(ctx, app.ID)
	if len(links) != 1 || links[0].Field != "password" || links[0].VarName != "DB_PASSWORD" {
		t.Fatalf("link not persisted with field: %+v", links)
	}

	// redis + field=dbname is rejected (redis has no dbname)
	postForm(t, h, base+"/db-links", cookie, url.Values{
		"db_ref": {"redis:" + i64(redis.ID)}, "var_name": {"R_DB"}, "scheme": {"redis"}, "field": {"dbname"},
	})
	if ls, _ := q.ListDBLinksByApplication(ctx, app.ID); len(ls) != 1 {
		t.Fatalf("redis dbname should be rejected, have %d links", len(ls))
	}

	// invalid field rejected
	postForm(t, h, base+"/db-links", cookie, url.Values{
		"db_ref": {"pg:" + i64(ld.ID)}, "var_name": {"BAD"}, "scheme": {"postgres"}, "field": {"bogus"},
	})
	if ls, _ := q.ListDBLinksByApplication(ctx, app.ID); len(ls) != 1 {
		t.Fatalf("invalid field should be rejected, have %d links", len(ls))
	}
}
