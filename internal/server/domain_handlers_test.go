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
		Env: map[string]string{}, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: a.ID, Host: primary, Tls: false, IsPrimary: true,
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
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid host want 400, got %d", rec.Code)
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
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate host want 400, got %d", rec.Code)
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
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete last domain want 400, got %d", rec.Code)
	}
	if _, err := q.GetDomain(ctx, last.ID); err != nil {
		t.Errorf("last domain %d was deleted: %v", last.ID, err)
	}
}
