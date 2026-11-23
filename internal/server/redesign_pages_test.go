package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// The registries and git credentials pages list the apps that use each one,
// and only the organization's own apps.
func TestCredentialsUsedBy(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	owner := mkUser(t, q, "usedby-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "UsedBy")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "storefront-api", Image: "nginx", Tag: "1", Domain: "usedby-api.example.com", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	idle, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "idle-app", Image: "nginx", Tag: "1", Domain: "usedby-idle.example.com", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{OrganizationID: o.ID, Name: "ghcr", RegistryUrl: "ghcr.io", Username: "bot", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	gc, err := q.CreateGitCredential(ctx, db.CreateGitCredentialParams{OrganizationID: o.ID, Name: "github", Host: "github.com", Username: "x-access-token", Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: app.ID, RegistryID: &reg.ID}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationGitCredential(ctx, db.SetApplicationGitCredentialParams{ID: app.ID, GitCredentialID: &gc.ID}); err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, q, "usedby-owner@k.local")
	base := "/orgs/" + i64(o.ID)
	appHref := base + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(app.ID)
	for _, page := range []string{"/registries", "/git-credentials"} {
		rec := getWithCookie(t, h, base+page, cookie)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `href="`+appHref+`?tab=settings#s-source"`) {
			t.Errorf("%s: the app using it is not listed (code %d)", page, rec.Code)
		}
		if strings.Contains(body, "idle-app") {
			t.Errorf("%s: an app that does not use it is listed", page)
		}
	}
	_ = idle

	// Another organization's member never sees these pages.
	mkUser(t, q, "usedby-stranger@k.local")
	stranger := loginAs(t, q, "usedby-stranger@k.local")
	if rec := getWithCookie(t, h, base+"/registries", stranger); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger got %d on another organization's registries", rec.Code)
	}
}

// The members page says which members still have to change their temporary
// password.
func TestMembersShowTemporaryPassword(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	owner := mkUser(t, q, "temp-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "Temp")
	fresh := mkUser(t, q, "temp-fresh@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: fresh, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, q, "temp-owner@k.local")
	path := "/orgs/" + i64(o.ID) + "/members"
	if body := getWithCookie(t, h, path, cookie).Body.String(); strings.Contains(body, "temporary password not changed yet") {
		t.Fatal("a member who changed nothing was shown as owing a password change")
	}
	if err := q.SetUserMustChangePassword(ctx, db.SetUserMustChangePasswordParams{ID: fresh, MustChangePassword: true}); err != nil {
		t.Fatal(err)
	}
	if body := getWithCookie(t, h, path, cookie).Body.String(); !strings.Contains(body, "temporary password not changed yet") {
		t.Fatal("the member who still owes a password change is not marked")
	}
}

// The palette's search only ever returns the organization's own things: an
// app of another organization never appears, not even through the env
// parameter, and instance-wide pages are offered to instance operators only.
func TestOrgSearchTenancy(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	userA := mkUser(t, q, "search-a@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userA, "SearchA")
	pA, _ := orgSvc.CreateProject(ctx, orgA.ID, "alpha", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "production")
	mkApp(t, q, eA.ID, "secret-billing", "search-a.example.com")

	userB := mkUser(t, q, "search-b@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userB, "SearchB")
	pB, _ := orgSvc.CreateProject(ctx, orgB.ID, "beta", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "production")
	mkApp(t, q, eB.ID, "billing-web", "search-b.example.com")
	member := mkUser(t, q, "search-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgB.ID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}

	cookieB := loginAs(t, q, "search-b@k.local")
	baseB := "/orgs/" + i64(orgB.ID) + "/search"
	rec := getWithCookie(t, h, baseB+"?q=billing&env="+i64(eA.ID), cookieB)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "billing-web") {
		t.Fatalf("own app missing from the results (code %d)", rec.Code)
	}
	if strings.Contains(body, "secret-billing") || strings.Contains(body, "alpha") {
		t.Fatal("SECURITY: another organization's app or project in the results")
	}
	// The organization's owner who is not an instance operator gets no
	// instance-wide pages.
	if body := getWithCookie(t, h, baseB+"?q=", cookieB).Body.String(); strings.Contains(body, "/nodes\"") || strings.Contains(body, "/monitoring\"") || strings.Contains(body, "/firewall\"") {
		t.Fatal("instance-wide pages offered to a non-operator")
	}
	// The environment's actions: an admin may create, a member may not.
	if body := getWithCookie(t, h, baseB+"?env="+i64(eB.ID), cookieB).Body.String(); !strings.Contains(body, "?new=app") {
		t.Fatal("the admin is not offered a new app in the environment")
	}
	memberCookie := loginAs(t, q, "search-member@k.local")
	if body := getWithCookie(t, h, baseB+"?env="+i64(eB.ID), memberCookie).Body.String(); strings.Contains(body, "?new=app") || !strings.Contains(body, "billing-web") {
		t.Fatal("a member must see the environment's apps and no create actions")
	}
	// A stranger is refused the search of an organization they do not belong to.
	cookieA := loginAs(t, q, "search-a@k.local")
	if rec := getWithCookie(t, h, baseB+"?q=billing", cookieA); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant search answered %d", rec.Code)
	}
}

// The palette finds databases and buckets with one organization-wide query
// each: another organization's never appear, and a % in the term is literal.
func TestOrgSearchResources(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	seed := func(email, orgName, ldb, bucket string) (int64, *http.Cookie) {
		u := mkUser(t, q, email)
		o, _ := orgSvc.CreateOrg(ctx, u, orgName)
		p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
		e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
		pg, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "postgres", Name: "main", AppName: "krill-postgres-" + orgName, Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: pg.ID, EnvironmentID: e.ID, Name: ldb, DbName: ldb, Username: ldb, Password: "x"}); err != nil {
			t.Fatal(err)
		}
		s3, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "minio", Name: "files", AppName: "krill-minio-" + orgName, Image: "pgsty/silo", Superuser: "root", SuperuserPassword: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateS3Bucket(ctx, db.CreateS3BucketParams{InstanceID: s3.ID, EnvironmentID: e.ID, Name: bucket, AccessKey: "k", SecretKey: "x"}); err != nil {
			t.Fatal(err)
		}
		// The palette also finds systems (L23 of the v0.13.0 audit).
		if _, err := q.CreateSystem(ctx, db.CreateSystemParams{EnvironmentID: e.ID, Name: bucket + "-system"}); err != nil {
			t.Fatal(err)
		}
		return o.ID, loginAs(t, q, email)
	}
	orgA, cookieA := seed("res-a@k.local", "resa", "orders_main", "orders-files")
	seed("res-b@k.local", "resb", "orders_secret", "orders-private")

	body := getWithCookie(t, h, "/orgs/"+i64(orgA)+"/search?q=orders", cookieA).Body.String()
	if !strings.Contains(body, "orders_main") || !strings.Contains(body, "orders-files") || !strings.Contains(body, "orders-files-system") {
		t.Fatalf("own database, bucket or system missing:\n%s", body)
	}
	if strings.Contains(body, "orders_secret") || strings.Contains(body, "orders-private") {
		t.Fatal("SECURITY: another organization's database, bucket or system in the results")
	}
	if body := getWithCookie(t, h, "/orgs/"+i64(orgA)+"/search?q=%25", cookieA).Body.String(); strings.Contains(body, "orders_main") {
		t.Fatal("a % in the search term matched everything")
	}
}

// The palette shows an app's live status, not the stored one.
func TestOrgSearchLiveStatus(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	u := mkUser(t, q, "live-palette@k.local")
	o, _ := orgSvc.CreateOrg(ctx, u, "LivePalette")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: e.ID, Name: "recovered-api", Image: "nginx", Tag: "1", Domain: "live-palette.example.com", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: a.ID, Status: "error"}); err != nil {
		t.Fatal(err)
	}
	// newDeployServer's engine reports every service running.
	body := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/search?q=recovered&env="+i64(e.ID), loginAs(t, q, "live-palette@k.local")).Body.String()
	if !strings.Contains(body, "recovered-api") {
		t.Fatalf("app missing:\n%s", body)
	}
	if strings.Contains(body, "state--err") {
		t.Fatal("the palette shows the stored error of an app Swarm reports running")
	}
}

// The API token page and an app's auto-deploy webhook show addresses a remote
// client can reach: the origin the operator is visiting when no public URL
// or panel domain is set — never the default KRILL_HOST, localhost.
func TestShownEndpointsUseTheVisitedOrigin(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	mkUser(t, q, "origin-owner@k.local")
	u, _ := q.GetUserByEmail(ctx, "origin-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, u.ID, "Origin")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: e.ID, Name: "api", Image: "nginx", Tag: "1", Domain: "origin-api.example.com", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationAutoDeploy(ctx, db.SetApplicationAutoDeployParams{ID: a.ID, AutoDeploy: true}); err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, q, "origin-owner@k.local")
	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "203.0.113.10:8080"
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	tokens := get("/orgs/" + i64(o.ID) + "/api-tokens")
	if !strings.Contains(tokens, "http://203.0.113.10:8080/api/v1") || !strings.Contains(tokens, "http://203.0.113.10:8080/mcp") || strings.Contains(tokens, "localhost") {
		t.Fatalf("the token page does not show the visited origin:\n%s", excerpt(tokens, "/api/v1"))
	}
	app := get("/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID) + "?tab=settings")
	if !strings.Contains(app, "http://203.0.113.10:8080/webhooks/deploy/"+i64(a.ID)) {
		t.Fatalf("the auto-deploy webhook does not use the visited origin:\n%s", excerpt(app, "/webhooks/"))
	}
}

// countingEngine counts cluster-wide service-state listings.
type countingEngine struct {
	noopEngine
	listings *int
}

func (e countingEngine) ServiceStates(ctx context.Context, names []string) (map[string]docker.ServiceState, error) {
	*e.listings++
	return e.noopEngine.ServiceStates(ctx, names)
}

// A palette search that shows no app does not ask Swarm for any status, and
// a server stays findable next to many matching databases.
func TestOrgSearchScopesItsWork(t *testing.T) {
	var listings int
	h, q, orgSvc, _, _ := buildDeployServer(t, nil, countingEngine{listings: &listings})
	ctx := context.Background()
	u := mkUser(t, q, "scope-palette@k.local")
	o, _ := orgSvc.CreateOrg(ctx, u, "ScopePalette")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	mkApp(t, q, e.ID, "storefront", "scope-palette.example.com")
	pg, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "postgres", Name: "main", AppName: "krill-postgres-scope", Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		n := "cache_" + strconv.Itoa(i)
		if _, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: pg.ID, EnvironmentID: e.ID, Name: n, DbName: n, Username: n, Password: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "redis", Name: "cache", AppName: "krill-redis-scope", Image: "redis:7", Superuser: "default", SuperuserPassword: "x"}); err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, q, "scope-palette@k.local")
	listings = 0
	body := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/search?q=cache", cookie).Body.String()
	if listings != 0 {
		t.Errorf("a search showing no app listed Swarm state %d times", listings)
	}
	if !strings.Contains(body, "/db-servers/") {
		t.Fatal("the cache server was pushed out by matching databases")
	}
	listings = 0
	if body := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/search?q=storefront", cookie).Body.String(); !strings.Contains(body, "storefront") || listings != 1 {
		t.Errorf("an app result must be looked up once (listings %d)", listings)
	}
}
