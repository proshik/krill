package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

// domainFixture builds an org→project→env→app chain owned by the admin, creates
// the app's primary domain directly, logs in as the owner, and returns the
// queries, the base app URL, and the owner's auth cookie.
func domainFixture(t *testing.T, h http.Handler, q *db.Queries, orgSvc *org.Service, primary string) (string, *http.Cookie, int64) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: primary, Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: a.ID, Host: primary, Tls: false, IsPrimary: true, Exposed: true, Paths: "",
	}); err != nil {
		t.Fatalf("create primary domain: %v", err)
	}
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID)
	return base, loginAs(t, q, "owner@k.local"), a.ID
}

func postForm(t *testing.T, h http.Handler, target string, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCreateAppCreatesPrimaryDomain verifies that a successful POST to createApp
// inserts exactly one primary domain row with IsPrimary=true, Tls=false, and
// Host = <name>.<BaseDomain>.
func TestCreateAppCreatesPrimaryDomain(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	ownerID := mkUser(t, q, "owner-a@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")

	appsURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps"
	cookie := loginAs(t, q, "owner-a@k.local")

	rec := postForm(t, h, appsURL, cookie, url.Values{
		"name":  {"myapp"},
		"image": {"nginx"},
		"tag":   {"latest"},
		"port":  {"8080"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("createApp want 303, got %d body: %s", rec.Code, rec.Body.String())
	}

	// Fetch the created application.
	apps, err := q.ListApplicationsByEnvironment(ctx, e.ID)
	if err != nil || len(apps) != 1 {
		t.Fatalf("expected 1 application, got %d err=%v", len(apps), err)
	}
	appID := apps[0].ID

	// Assert exactly one primary domain row.
	doms, err := q.ListDomainsByApplication(ctx, appID)
	if err != nil {
		t.Fatalf("ListDomainsByApplication: %v", err)
	}
	if len(doms) != 1 {
		t.Fatalf("expected exactly 1 domain, got %d: %+v", len(doms), doms)
	}
	d := doms[0]
	wantHost := "myapp.127-0-0-1.sslip.io"
	if d.Host != wantHost {
		t.Errorf("domain Host want %q, got %q", wantHost, d.Host)
	}
	if !d.IsPrimary {
		t.Errorf("domain IsPrimary want true, got false")
	}
	if d.Tls {
		t.Errorf("domain Tls want false, got true")
	}
	if d.ApplicationID != appID {
		t.Errorf("domain ApplicationID want %d, got %d", appID, d.ApplicationID)
	}
}

func TestCreateAppCustomDomainValidation(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	ownerID := mkUser(t, q, "owner-a@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	appsURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps"
	cookie := loginAs(t, q, "owner-a@k.local")

	base := url.Values{"image": {"nginx"}, "tag": {"latest"}, "port": {"8080"}}

	// Invalid custom domain (would otherwise be injected unescaped into a Traefik rule) -> err flash + 303.
	bad := url.Values{"name": {"app1"}, "domain": {"x.com`)||PathPrefix(`/"}}
	for k, v := range base {
		bad[k] = v
	}
	if rec := postForm(t, h, appsURL, cookie, bad); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("invalid custom domain want 303+err flash, got %d flash=%q", rec.Code, flashCookieValue(rec))
	}

	// First app claims a custom domain.
	ok1 := url.Values{"name": {"app2"}, "domain": {"taken.example.com"}}
	for k, v := range base {
		ok1[k] = v
	}
	if rec := postForm(t, h, appsURL, cookie, ok1); rec.Code != http.StatusSeeOther {
		t.Fatalf("first custom domain want 303, got %d body: %s", rec.Code, rec.Body.String())
	}

	// Second app reusing the same host -> err flash + 303, and no orphan app is created.
	dup := url.Values{"name": {"app3"}, "domain": {"taken.example.com"}}
	for k, v := range base {
		dup[k] = v
	}
	if rec := postForm(t, h, appsURL, cookie, dup); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("duplicate custom domain want 303+err flash, got %d flash=%q", rec.Code, flashCookieValue(rec))
	}
	apps, err := q.ListApplicationsByEnvironment(ctx, e.ID)
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("expected exactly 1 app (no orphan), got %d", len(apps))
	}
}

func TestAddDomain(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "web.primary.example.com")

	rec := postForm(t, h, base+"/domains", cookie, url.Values{"host": {"app-x.example.com"}, "tls": {"on"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add domain want 303, got %d", rec.Code)
	}

	doms, _ := q.ListDomainsByApplication(ctx, appID)
	var found *db.Domain
	for i := range doms {
		if doms[i].Host == "app-x.example.com" {
			found = &doms[i]
		}
	}
	if found == nil {
		t.Fatalf("added domain not found; got %+v", doms)
	}
	if !found.Tls {
		t.Errorf("added domain Tls want true, got false")
	}
}

func TestAddDomainInvalidHost(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "web.primary.example.com")

	rec := postForm(t, h, base+"/domains", cookie, url.Values{"host": {"not a host"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("invalid host want 303, got %d", rec.Code)
	}
	if !hasErrFlash(rec) {
		t.Fatalf("invalid host want err flash, got %q", flashCookieValue(rec))
	}

	doms, _ := q.ListDomainsByApplication(ctx, appID)
	if len(doms) != 1 {
		t.Fatalf("expected only the primary domain, got %d: %+v", len(doms), doms)
	}
}

func TestAddDomainDuplicate(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "web.primary.example.com")

	// add once — should succeed
	if rec := postForm(t, h, base+"/domains", cookie, url.Values{"host": {"dup.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("first add want 303, got %d", rec.Code)
	}
	// add the same host again — should fail
	rec := postForm(t, h, base+"/domains", cookie, url.Values{"host": {"dup.example.com"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("duplicate host want 303, got %d", rec.Code)
	}
	if !hasErrFlash(rec) {
		t.Fatalf("duplicate host want err flash, got %q", flashCookieValue(rec))
	}

	doms, _ := q.ListDomainsByApplication(ctx, appID)
	if len(doms) != 2 { // primary + one dup.example.com
		t.Fatalf("expected 2 domains after duplicate attempt, got %d: %+v", len(doms), doms)
	}
}

func TestToggleDomainTLS(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "web.primary.example.com")

	// add a non-primary domain (tls off)
	if rec := postForm(t, h, base+"/domains", cookie, url.Values{"host": {"toggle.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add want 303, got %d", rec.Code)
	}
	doms, _ := q.ListDomainsByApplication(ctx, appID)
	var target db.Domain
	for _, d := range doms {
		if d.Host == "toggle.example.com" {
			target = d
		}
	}
	if target.ID == 0 {
		t.Fatalf("toggle.example.com not found; got %+v", doms)
	}
	before := target.Tls

	rec := postForm(t, h, base+"/domains/"+i64(target.ID)+"/tls", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("toggle tls want 303, got %d", rec.Code)
	}
	after, _ := q.GetDomain(ctx, target.ID)
	if after.Tls == before {
		t.Errorf("Tls did not flip: before=%v after=%v", before, after.Tls)
	}
}

func TestDeleteDomain(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "web.primary.example.com")

	// add a second domain so deletion is allowed
	if rec := postForm(t, h, base+"/domains", cookie, url.Values{"host": {"second.example.com"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add want 303, got %d", rec.Code)
	}
	doms, _ := q.ListDomainsByApplication(ctx, appID)
	var target db.Domain
	for _, d := range doms {
		if d.Host == "second.example.com" {
			target = d
		}
	}
	if target.ID == 0 {
		t.Fatalf("second.example.com not found; got %+v", doms)
	}

	rec := postForm(t, h, base+"/domains/"+i64(target.ID)+"/delete", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete want 303, got %d", rec.Code)
	}
	if _, err := q.GetDomain(ctx, target.ID); err == nil {
		t.Errorf("domain %d still present after delete", target.ID)
	}
}

func TestCannotDeleteLastDomain(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "web.primary.example.com")

	doms, _ := q.ListDomainsByApplication(ctx, appID)
	if len(doms) != 1 {
		t.Fatalf("expected exactly 1 domain at start, got %d", len(doms))
	}
	last := doms[0]

	rec := postForm(t, h, base+"/domains/"+i64(last.ID)+"/delete", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete last domain want 303, got %d", rec.Code)
	}
	if !hasErrFlash(rec) {
		t.Fatalf("delete last domain want err flash, got %q", flashCookieValue(rec))
	}
	if _, err := q.GetDomain(ctx, last.ID); err != nil {
		t.Errorf("last domain %d was deleted: %v", last.ID, err)
	}
}

// exposureAppFixture builds an org→project→env chain and creates an app via the
// real createApp POST flow (so the primary domain gets the createApp default
// Exposed=false), then returns the base app URL, the owner cookie, the app ID,
// and the primary domain ID.
func exposureAppFixture(t *testing.T, h http.Handler, q *db.Queries, orgSvc *org.Service, name, email string) (string, *http.Cookie, int64, int64) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, email)
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	appsURL := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps"
	cookie := loginAs(t, q, email)

	rec := postForm(t, h, appsURL, cookie, url.Values{
		"name":  {name},
		"image": {"nginx"},
		"tag":   {"latest"},
		"port":  {"8080"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("createApp want 303, got %d body: %s", rec.Code, rec.Body.String())
	}
	apps, err := q.ListApplicationsByEnvironment(ctx, e.ID)
	if err != nil || len(apps) != 1 {
		t.Fatalf("expected 1 application, got %d err=%v", len(apps), err)
	}
	appID := apps[0].ID
	doms, err := q.ListDomainsByApplication(ctx, appID)
	if err != nil {
		t.Fatalf("ListDomainsByApplication: %v", err)
	}
	var primaryID int64
	for _, d := range doms {
		if d.IsPrimary {
			primaryID = d.ID
		}
	}
	if primaryID == 0 {
		t.Fatalf("primary domain not found; got %+v", doms)
	}
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(appID)
	return base, cookie, appID, primaryID
}

// TestSetDomainExposureInvalidPath verifies that an invalid path (missing
// leading slash) is rejected with an err flash and leaves the domain unchanged.
func TestSetDomainExposureInvalidPath(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, primaryID := exposureAppFixture(t, h, q, orgSvc, "expapp", "owner-exp1@k.local")

	rec := postForm(t, h, base+"/domains/"+i64(primaryID)+"/exposure", cookie, url.Values{
		"exposed": {"on"},
		"paths":   {"bad-no-slash"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("invalid path want 303, got %d", rec.Code)
	}
	if !hasErrFlash(rec) {
		t.Fatalf("invalid path want err flash, got %q", flashCookieValue(rec))
	}
	d, err := q.GetDomain(ctx, primaryID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if d.Exposed {
		t.Errorf("Exposed want false (unchanged), got true")
	}
}

// TestSetDomainExposureValid verifies that a valid exposure update persists
// Exposed=true and the cleaned path.
func TestSetDomainExposureValid(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, primaryID := exposureAppFixture(t, h, q, orgSvc, "expapp", "owner-exp2@k.local")

	rec := postForm(t, h, base+"/domains/"+i64(primaryID)+"/exposure", cookie, url.Values{
		"exposed": {"on"},
		"paths":   {"/webhook"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid exposure want 303, got %d", rec.Code)
	}
	d, err := q.GetDomain(ctx, primaryID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if !d.Exposed {
		t.Errorf("Exposed want true, got false")
	}
	if d.Paths != "/webhook" {
		t.Errorf("Paths want %q, got %q", "/webhook", d.Paths)
	}
}

// TestCreateAppPrimaryDomainInternal verifies that the primary domain created by
// the createApp flow is internal by default (Exposed=false).
func TestCreateAppPrimaryDomainInternal(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	_, _, _, primaryID := exposureAppFixture(t, h, q, orgSvc, "expapp", "owner-exp3@k.local")

	d, err := q.GetDomain(ctx, primaryID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if d.Exposed {
		t.Errorf("primary domain Exposed want false (internal default), got true")
	}
}

// TestDomainCrossTenantIsolation verifies that a user authenticated under
// org-B cannot mutate (toggle TLS or delete) a domain that belongs to org-A's
// application by injecting org-A's domainID into the org-B URL path.
// Any non-404 response is treated as a security finding and fails the test.
func TestDomainCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	// --- Org-A: owner userA, project, environment, application, extra domain ---
	userAID := mkUser(t, q, "usera-dom@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userAID, "OrgA-Dom")
	projA, _ := orgSvc.CreateProject(ctx, orgA.ID, "ProjA-Dom", "")
	envA, _ := orgSvc.CreateEnvironment(ctx, projA.ID, "prod-a-dom")
	appA, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID:  envA.ID,
		Name:           "web-a",
		Image:          "nginx",
		Tag:            "alpine",
		Domain:         "web-a.primary.example.com",
		Port:           80,
		SourceType:     "image",
		GitUrl:         "",
		GitBranch:      "",
		DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create org-A application: %v", err)
	}
	// primary domain for app-A (required for the app to be valid)
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: appA.ID, Host: "web-a.primary.example.com", Tls: false, IsPrimary: true, Exposed: true, Paths: "",
	}); err != nil {
		t.Fatalf("create org-A primary domain: %v", err)
	}
	// extra non-primary domain — this is the one we will try to steal
	extraDom, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: appA.ID, Host: "a-extra.example.com", Tls: false, IsPrimary: false, Exposed: true, Paths: "",
	})
	if err != nil {
		t.Fatalf("create org-A extra domain: %v", err)
	}
	aDomainID := extraDom.ID

	// --- Org-B: owner userB, project, environment, application ---
	userBID := mkUser(t, q, "userb-dom@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userBID, "OrgB-Dom")
	projB, _ := orgSvc.CreateProject(ctx, orgB.ID, "ProjB-Dom", "")
	envB, _ := orgSvc.CreateEnvironment(ctx, projB.ID, "prod-b-dom")
	appB, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID:  envB.ID,
		Name:           "web-b",
		Image:          "nginx",
		Tag:            "alpine",
		Domain:         "web-b.primary.example.com",
		Port:           80,
		SourceType:     "image",
		GitUrl:         "",
		GitBranch:      "",
		DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create org-B application: %v", err)
	}

	cookieB := loginAs(t, q, "userb-dom@k.local")

	// Org-B's app path, but with org-A's domainID injected.
	bAppBase := "/orgs/" + i64(orgB.ID) +
		"/projects/" + i64(projB.ID) +
		"/environments/" + i64(envB.ID) +
		"/apps/" + i64(appB.ID)

	routes := []struct {
		suffix string
	}{
		{"/domains/" + i64(aDomainID) + "/tls"},
		{"/domains/" + i64(aDomainID) + "/delete"},
	}

	for _, tc := range routes {
		target := bAppBase + tc.suffix
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookieB)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("SECURITY FINDING: POST %s — expected 404 (cross-tenant isolation), got %d", target, rec.Code)
		}
	}

	// After both attempts org-A's extra domain must still exist.
	if _, err := q.GetDomain(ctx, aDomainID); err != nil {
		t.Fatalf("SECURITY FINDING: org-A domain (id=%d) was deleted by cross-tenant request: %v", aDomainID, err)
	}
}
