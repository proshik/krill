package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/secret"
)

type provisionFixture struct {
	h        http.Handler
	q        *db.Queries
	orgID    int64
	envID    int64
	envPath  string // "proj/prod"
	appID    int64
	instID   int64 // a Postgres server of the org
	cacheID  int64 // a Redis server of the org
	provTok  string
	writeTok string
	readTok  string
}

func seedProvision(t *testing.T) provisionFixture {
	t.Helper()
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	owner := mkUser(t, q, "prov-owner@k.local")
	o, err := orgSvc.CreateOrg(ctx, owner, "ProvOrg")
	if err != nil {
		t.Fatal(err)
	}
	p, err := orgSvc.CreateProject(ctx, o.ID, "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := orgSvc.CreateEnvironment(ctx, p.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "prov-web.example.test", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: app.ID, Host: "prov-web.example.test", IsPrimary: true}); err != nil {
		t.Fatal(err)
	}
	pg, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "postgres", Name: "main", AppName: "krill-postgres-main-p1", Image: "postgres:17", Superuser: "postgres", SuperuserPassword: secret.Enc("x")})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "redis", Name: "cache", AppName: "krill-redis-cache-p1", Image: "redis:7", Superuser: "default", SuperuserPassword: secret.Enc("x")})
	if err != nil {
		t.Fatal(err)
	}
	return provisionFixture{
		h: h, q: q, orgID: o.ID, envID: e.ID, envPath: "proj/prod", appID: app.ID, instID: pg.ID, cacheID: cache.ID,
		provTok:  issueAPIToken(t, q, owner, o.ID, api.LevelProvision),
		writeTok: issueAPIToken(t, q, owner, o.ID, api.LevelWrite),
		readTok:  issueAPIToken(t, q, owner, o.ID, api.LevelRead),
	}
}

func provCall(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
}

func provRoutes(fx provisionFixture, appRef string) []struct{ path, body string } {
	return []struct{ path, body string }{
		{"/api/v1/environments/" + fx.envPath + "/databases", `{"name":"x","server":"main"}`},
		{"/api/v1/environments/" + fx.envPath + "/apps", `{"name":"x","image":"nginx","port":80}`},
		{"/api/v1/apps/" + appRef + "/domains", `{"host":"x.example.test"}`},
		{"/api/v1/apps/" + appRef + "/db-links", `{"var":"REDIS_URL","server":"cache"}`},
	}
}

// Creating needs a provision-level token: read and write tokens — including
// the CI write tokens that exist today — are refused before anything is
// resolved, and nothing is created.
func TestProvisionRequiresProvisionLevel(t *testing.T) {
	fx := seedProvision(t)
	for _, tok := range []string{fx.readTok, fx.writeTok} {
		for _, rt := range provRoutes(fx, "proj/prod/web") {
			rec := provCall(t, fx.h, tok, rt.path, rt.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: want 403, got %d: %s", rt.path, rec.Code, rec.Body.String())
			}
		}
	}
	apps, _ := fx.q.ListApplicationsByEnvironment(context.Background(), fx.envID)
	if len(apps) != 1 {
		t.Fatalf("a refused request created an app: %d apps", len(apps))
	}
}

// A provision token whose owner is demoted to member loses the right at once.
func TestProvisionFollowsTheOwnersLiveRole(t *testing.T) {
	fx := seedProvision(t)
	ctx := context.Background()
	member := mkUser(t, fx.q, "prov-member@k.local")
	if _, err := fx.q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: fx.orgID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	tok := issueAPIToken(t, fx.q, member, fx.orgID, api.LevelProvision)
	rec := provCall(t, fx.h, tok, "/api/v1/environments/"+fx.envPath+"/apps", `{"name":"x","image":"nginx","port":80}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member's provision token: want 403, got %d", rec.Code)
	}
}

// Another organization's environment or app answers not found, never
// forbidden, even to a provision token.
func TestProvisionRejectsForeignOrg(t *testing.T) {
	fx := seedProvision(t)
	other := mkUser(t, fx.q, "prov-other@k.local")
	o2, err := org.NewService(fx.q).CreateOrg(context.Background(), other, "OtherOrg")
	if err != nil {
		t.Fatal(err)
	}
	tok := issueAPIToken(t, fx.q, other, o2.ID, api.LevelProvision)
	for _, rt := range provRoutes(fx, itoaTest(fx.appID)) {
		rec := provCall(t, fx.h, tok, rt.path, rt.body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d: %s", rt.path, rec.Code, rec.Body.String())
		}
	}
}

func itoaTest(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Creating an app is idempotent: a repeat returns the same app with
// created=false, a different image under the same name is a conflict, and the
// primary domain gets the requested exposure and HTTPS.
func TestProvisionCreateAppIdempotent(t *testing.T) {
	fx := seedProvision(t)
	path := "/api/v1/environments/" + fx.envPath + "/apps"
	body := `{"name":"bot","image":"ghcr.io/acme/bot","tag":"v1","port":8080,"domain":{"host":"bot.example.test","tls":true,"exposed":true}}`
	var first, again api.AppResult
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, body), &first)
	if !first.Created || first.Path != "proj/prod/bot" || first.Domain != "bot.example.test" {
		t.Fatalf("first = %+v", first)
	}
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, body), &again)
	if again.Created || again.ID != first.ID {
		t.Fatalf("repeat = %+v, want the same app with created=false", again)
	}
	rec := provCall(t, fx.h, fx.provTok, path, `{"name":"bot","image":"ghcr.io/acme/other","port":8080,"domain":{"host":"bot.example.test","tls":true,"exposed":true}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("different image: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	app, err := fx.q.GetApplication(context.Background(), first.ID)
	if err != nil || app.Image != "ghcr.io/acme/bot" || app.Tag != "v1" || app.Port != 8080 || app.SourceType != "image" {
		t.Fatalf("stored app = %+v err=%v", app, err)
	}
	doms, _ := fx.q.ListDomainsByApplication(context.Background(), first.ID)
	if len(doms) != 1 || !doms[0].IsPrimary || !doms[0].Tls || !doms[0].Exposed {
		t.Fatalf("primary domain = %+v", doms)
	}
	// The host is now taken: another app cannot claim it.
	rec = provCall(t, fx.h, fx.provTok, path, `{"name":"bot2","image":"nginx","port":80,"domain":{"host":"bot.example.test"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("taken host: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Without a domain, a new app gets <name>.<base domain>, internal.
func TestProvisionCreateAppDefaultDomainIsInternal(t *testing.T) {
	fx := seedProvision(t)
	var res api.AppResult
	decodeInto(t, provCall(t, fx.h, fx.provTok, "/api/v1/environments/"+fx.envPath+"/apps", `{"name":"svc","image":"nginx","port":80}`), &res)
	if res.Domain != "svc.127-0-0-1.sslip.io" {
		t.Fatalf("domain = %q", res.Domain)
	}
	doms, _ := fx.q.ListDomainsByApplication(context.Background(), res.ID)
	if len(doms) != 1 || doms[0].Exposed || doms[0].Tls {
		t.Fatalf("default domain = %+v, want internal without HTTPS", doms)
	}
}

func TestProvisionAddDomainIdempotent(t *testing.T) {
	fx := seedProvision(t)
	path := "/api/v1/apps/proj/prod/web/domains"
	body := `{"host":"www.example.test","tls":true,"exposed":true,"paths":["/webhook"]}`
	var first, again api.DomainResult
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, body), &first)
	if !first.Created || first.Primary || !first.TLS || !first.Exposed || len(first.Paths) != 1 || first.Paths[0] != "/webhook" {
		t.Fatalf("first = %+v", first)
	}
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, body), &again)
	if again.Created || again.ID != first.ID {
		t.Fatalf("repeat = %+v", again)
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"host":"www.example.test"}`); rec.Code != http.StatusConflict {
		t.Fatalf("different settings: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"host":"not a host"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad host: want 400, got %d", rec.Code)
	}
}

// A database that already exists under the name answers as a repeat; with
// other settings it is a conflict; a Redis server holds no logical databases.
func TestProvisionCreateDatabaseIdempotent(t *testing.T) {
	fx := seedProvision(t)
	ctx := context.Background()
	if _, err := fx.q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: fx.instID, EnvironmentID: fx.envID, Name: "app", DbName: "app", Username: "app", Password: secret.Enc("pw")}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/environments/" + fx.envPath + "/databases"
	var res api.DatabaseResult
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, `{"name":"app","server":"main"}`), &res)
	if res.Created || res.DBName != "app" || res.Server != "main" {
		t.Fatalf("repeat = %+v", res)
	}
	if strings.Contains(res.Name, "pw") {
		t.Fatal("password leaked")
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"name":"app","server":"main","db_name":"other"}`); rec.Code != http.StatusConflict {
		t.Fatalf("different db_name: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"name":"x","server":"cache"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("redis server: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"name":"x","server":"missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server: want 404, got %d", rec.Code)
	}
	// A new one is created (the test engine accepts the provisioning exec),
	// its password stays encrypted and is never returned, and a repeat is a
	// no-op.
	var fresh, freshAgain api.DatabaseResult
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, `{"name":"fresh","server":"main"}`), &fresh)
	if !fresh.Created || fresh.DBName != "fresh" || fresh.Username != "fresh" {
		t.Fatalf("fresh = %+v", fresh)
	}
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, `{"name":"fresh","server":"main"}`), &freshAgain)
	if freshAgain.Created || freshAgain.ID != fresh.ID {
		t.Fatalf("repeat = %+v", freshAgain)
	}
	ld, err := fx.q.GetLogicalDatabase(ctx, fresh.ID)
	if err != nil || ld.Password == "" {
		t.Fatalf("stored row = %+v err=%v", ld, err)
	}
}

func TestProvisionLinkDatabaseIdempotent(t *testing.T) {
	fx := seedProvision(t)
	ctx := context.Background()
	if _, err := fx.q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: fx.instID, EnvironmentID: fx.envID, Name: "app", DbName: "app", Username: "app", Password: secret.Enc("pw")}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/apps/proj/prod/web/db-links"
	var first, again api.LinkResult
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, `{"var":"DATABASE_URL","database":"app"}`), &first)
	if !first.Created || first.Var != "DATABASE_URL" || first.Field != "url" {
		t.Fatalf("first = %+v", first)
	}
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, `{"var":"DATABASE_URL","database":"app"}`), &again)
	if again.Created || again.ID != first.ID {
		t.Fatalf("repeat = %+v", again)
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"var":"DATABASE_URL","server":"cache"}`); rec.Code != http.StatusConflict {
		t.Fatalf("var linked elsewhere: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var redis api.LinkResult
	decodeInto(t, provCall(t, fx.h, fx.provTok, path, `{"var":"REDIS_URL","server":"cache"}`), &redis)
	if !redis.Created || redis.Target != "cache" {
		t.Fatalf("redis link = %+v", redis)
	}
	if err := fx.q.UpdateApplicationEnv(ctx, db.UpdateApplicationEnvParams{ID: fx.appID, EnvText: "TAKEN=1"}); err != nil {
		t.Fatal(err)
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"var":"TAKEN","server":"cache"}`); rec.Code != http.StatusConflict {
		t.Fatalf("var set in env: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := provCall(t, fx.h, fx.provTok, path, `{"var":"X","database":"app","server":"cache"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("both targets: want 400, got %d", rec.Code)
	}
	links, _ := fx.q.ListDBLinksByApplication(ctx, fx.appID)
	if len(links) != 2 {
		t.Fatalf("links = %d, want 2", len(links))
	}
}

// A provision token can do everything a write token can.
func TestProvisionTokenCanWrite(t *testing.T) {
	fx := seedProvision(t)
	rec := provCall(t, fx.h, fx.provTok, "/api/v1/apps/proj/prod/web/env", `{"key":"K","value":"V"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set env with a provision token: %d %s", rec.Code, rec.Body.String())
	}
}
