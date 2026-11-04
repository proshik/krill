package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/envclone"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

func TestEnvironmentCloneHTTP(t *testing.T) {
	ctx := t.Context()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	os := org.NewService(q)
	owner := mkUser(t, q, "clone-owner@fixture.test")
	o, err := os.CreateOrg(ctx, owner, "clone-org")
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.CreateProject(ctx, o.ID, "project", "")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	hub := deploy.NewLogHub()
	dbsvc := dbservice.New(nil, dbservice.NewDBStore(q), hub, "unused")
	cloner := envclone.New(pool, dbsvc, nil, "clone.test")
	defer cloner.Shutdown(context.Background())
	cfg := config.Config{BaseDomain: "clone.test"}
	srv := server.New(cfg, auth.NewService(q), os, q, nil, nil, hub, dbsvc)
	srv.SetHostShare(hostshare.NewStore(pool))
	srv.SetEnvironmentCloner(cloner)
	h := srv.Router()
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(src.ID)
	cookie := loginAs(t, q, "clone-owner@fixture.test")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: src.ID, Name: "web", SourceType: "image", Image: "http-app", Tag: "stable", Port: 8080, Domain: "production.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: app.ID, EnvText: "# comment\nPUBLIC=https://production.test\nTOKEN=source-secret\nCACHE=source-dsn", EnvPublicKeys: []string{"PUBLIC", "CACHE"}}); err != nil {
		t.Fatal(err)
	}
	oldToken := "source-metrics-token"
	if err := q.SetApplicationMetricsToken(ctx, db.SetApplicationMetricsTokenParams{ID: app.ID, MetricsToken: &oldToken}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationMetricsEnabled(ctx, db.SetApplicationMetricsEnabledParams{ID: app.ID, MetricsEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: app.ID, Port: 8080, Path: "/metrics", Job: "web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data"}); err != nil {
		t.Fatal(err)
	}
	for index, engine := range []string{"redis", "dragonfly", "minio"} {
		port := int32(15000 + index)
		in, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: engine, Name: engine, AppName: "source-" + engine, Image: engine + ":fixture", Superuser: "sourceuser", SuperuserPassword: "source-server-secret", ExternalPort: &port})
		if err != nil {
			t.Fatal(err)
		}
		field, scheme := "url", "redis"
		if engine == "minio" {
			field, scheme = "endpoint", "redis"
		}
		variable := strings.ToUpper(engine)
		if engine == "redis" {
			variable = "CACHE"
		}
		if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, InstanceID: &in.ID, VarName: variable, Field: field, Scheme: scheme}); err != nil {
			t.Fatal(err)
		}
		if engine == "redis" {
			if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: app.ID, InstanceID: &in.ID, VarName: "CACHE_HOST", Field: "host", Scheme: "redis"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	get := httptest.NewRequest("GET", base, nil)
	get.AddCookie(cookie)
	page := httptest.NewRecorder()
	h.ServeHTTP(page, get)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `id="clone-env"`) || !strings.Contains(page.Body.String(), `value="staging"`) {
		t.Fatal("clone modal missing")
	}
	for _, secret := range []string{"source-secret", "source-dsn", "source-metrics-token", "source-server-secret"} {
		if strings.Contains(page.Body.String(), secret) {
			t.Fatal("read-only clone flow exposed source secret")
		}
	}
	key, err := envclone.NewRequestKey()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"name": {"staging"}, "request_key": {key}}
	result := postForm(t, h, base+"/clone", cookie, form)
	if result.Code != http.StatusSeeOther || hasErrFlash(result) {
		t.Fatalf("clone refused: %s", flashText(result))
	}
	targetURL := result.Header().Get("Location")
	envs, err := q.ListEnvironments(ctx, p.ID)
	if err != nil || len(envs) != 2 {
		t.Fatal("clone environment not published")
	}
	target := envs[1]
	apps, err := q.ListApplicationsByEnvironment(ctx, target.ID)
	if err != nil || len(apps) != 1 {
		t.Fatal("cloned app missing")
	}
	a := apps[0]
	if a.Status != "idle" || a.AutoDeploy || a.WebhookSecret != "" || a.EnvText != "# comment\nPUBLIC=https://production.test\nTOKEN=\nCACHE=" {
		t.Fatal("unsafe clone settings")
	}
	if a.MetricsToken == nil || *a.MetricsToken == oldToken || !a.MetricsEnabled {
		t.Fatal("metrics identity not isolated")
	}
	domains, err := q.ListDomainsByApplication(ctx, a.ID)
	if err != nil || len(domains) != 1 || domains[0].Exposed || domains[0].Tls || domains[0].Host == "production.test" {
		t.Fatal("domain exposure copied")
	}
	links, err := q.ListDBLinksByApplication(ctx, a.ID)
	if err != nil || len(links) != 4 {
		t.Fatal("links missing")
	}
	instances, err := q.ListDBInstancesByOrg(ctx, o.ID)
	if err != nil || len(instances) != 6 {
		t.Fatal("shared source instance cloned more than once")
	}
	for _, link := range links {
		in, err := q.GetDBInstance(ctx, *link.InstanceID)
		if err != nil || strings.HasPrefix(in.AppName, "source-") || in.SuperuserPassword == "source-server-secret" || in.ExternalPort != nil || in.ConsoleExternalPort != nil || in.Status != "idle" {
			t.Fatal("non-PG clone not isolated/undeployed")
		}
	}
	if retry := postForm(t, h, base+"/clone", cookie, form); retry.Header().Get("Location") != targetURL || hasErrFlash(retry) {
		t.Fatal("same request not idempotent")
	}
	another, _ := envclone.NewRequestKey()
	if conflict := postForm(t, h, base+"/clone", cookie, url.Values{"name": {"staging"}, "request_key": {another}}); !hasErrFlash(conflict) {
		t.Fatal("same-name environment reused")
	}
	member := mkUser(t, q, "clone-member@fixture.test")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if denied := postForm(t, h, base+"/clone", loginAs(t, q, "clone-member@fixture.test"), form); denied.Code != 403 {
		t.Fatal("member allowed to clone")
	}
	foreign, err := os.CreateProject(ctx, o.ID, "foreign", "")
	if err != nil {
		t.Fatal(err)
	}
	bad := "/orgs/" + i64(o.ID) + "/projects/" + i64(foreign.ID) + "/environments/" + i64(src.ID) + "/clone"
	if denied := postForm(t, h, bad, cookie, form); denied.Code != 404 {
		t.Fatal("mismatched project chain accepted")
	}

	t.Run("tenant mismatch wins over held clone guard", func(t *testing.T) {
		key := oplock.CloneProject(foreign.ID)
		if !oplock.TryAcquire(key) {
			t.Fatal("fixture guard busy")
		}
		defer oplock.Release(key)
		wrong := strings.TrimSuffix(bad, "/clone") + "/delete"
		if rec := postForm(t, h, wrong, cookie, url.Values{}); rec.Code != 404 {
			t.Fatalf("mismatch while busy: %d", rec.Code)
		}
		if _, err := q.GetEnvironment(ctx, src.ID); err != nil {
			t.Fatal("mismatch deleted source")
		}
	})
	t.Run("settings storage failure differs from lifecycle busy", func(t *testing.T) {
		in := instances[0]
		path := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(in.ID)
		key := oplock.DBInstanceDeploy(in.ID)
		if !oplock.TryAcquire(key) {
			t.Fatal("fixture guard busy")
		}
		busy := postForm(t, h, path+"/version", cookie, url.Values{"image": {"redis:fixture"}})
		oplock.Release(key)
		if !strings.Contains(flashText(busy), "already running") {
			t.Fatal("busy not categorized")
		}
		_, err := pool.Exec(ctx, `CREATE FUNCTION reject_clone_fixture_settings() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture settings failure'; END $$;
 CREATE TRIGGER reject_clone_fixture_settings BEFORE UPDATE OF image,external_port ON db_instances FOR EACH ROW EXECUTE FUNCTION reject_clone_fixture_settings()`)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Exec(ctx, "DROP TRIGGER reject_clone_fixture_settings ON db_instances; DROP FUNCTION reject_clone_fixture_settings()")
		for _, action := range []string{"version", "external-port"} {
			rec := postForm(t, h, path+"/"+action, cookie, url.Values{"image": {"redis:fixture"}, "external_port": {""}})
			if !strings.Contains(flashText(rec), "internal error") || strings.Contains(flashText(rec), "fixture settings failure") {
				t.Fatal("storage failure misclassified or exposed")
			}
		}
	})
	// Reopening the router and reloading preserves the warning and saved config.
	reload := httptest.NewRequest("GET", targetURL, nil)
	reload.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reload)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "data-clone-notice") {
		t.Fatal("clone review help did not persist")
	}
	if deleted := postForm(t, h, targetURL+"/delete", cookie, url.Values{}); hasErrFlash(deleted) {
		t.Fatal("clone deletion failed")
	}
	instances, err = q.ListDBInstancesByOrg(ctx, o.ID)
	if err != nil || len(instances) != 6 {
		t.Fatal("environment deletion destroyed org-level cloned servers")
	}
}
