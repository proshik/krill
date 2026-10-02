package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

func orgServiceFor(q *db.Queries) *org.Service { return org.NewService(q) }

// shareEnv: one environment holding "web" (exposed catch-all primary on
// m.example.com) and "api" (its own primary). Uses the existing helpers
// mkUser, loginAs, i64, postForm, hasErrFlash, flashCookieValue, newServer.
type shareEnv struct {
	h       http.Handler
	q       *db.Queries
	cookie  *http.Cookie
	webURL  string
	apiURL  string
	webID   int64
	apiID   int64
	envBase string
}

func newShareEnv(t *testing.T) shareEnv {
	t.Helper()
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	owner := mkUser(t, q, "share-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	mk := func(name, host string) int64 {
		a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
			EnvironmentID: e.ID, Name: name, Image: "nginx", Tag: "alpine", Domain: host, Port: 80,
			SourceType: "image", DockerfilePath: "Dockerfile",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: a.ID, Host: host, IsPrimary: true, Exposed: true}); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	webID := mk("web", "m.example.com")
	apiID := mk("api", "api.primary.example.com")
	envBase := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID)
	return shareEnv{h: h, q: q, cookie: loginAs(t, q, "share-owner@k.local"),
		webURL: envBase + "/apps/" + i64(webID), apiURL: envBase + "/apps/" + i64(apiID),
		webID: webID, apiID: apiID, envBase: envBase}
}

func TestShareHostInOneEnvironment(t *testing.T) {
	s := newShareEnv(t)
	rec := postForm(t, s.h, s.apiURL+"/domains", s.cookie, url.Values{"host": {"m.example.com"}, "exposed": {"on"}, "paths": {"/api"}})
	if hasErrFlash(rec) {
		t.Fatalf("sharing refused: %q", flashText(rec))
	}
	doms, _ := s.q.ListDomainsByApplication(context.Background(), s.apiID)
	found := false
	for _, d := range doms {
		if d.Host == "m.example.com" && d.Paths == "/api" {
			found = true
		}
	}
	if !found {
		t.Fatalf("shared row missing: %+v", doms)
	}
}

func TestShareHostSecondCatchAllNamesTheApp(t *testing.T) {
	s := newShareEnv(t)
	rec := postForm(t, s.h, s.apiURL+"/domains", s.cookie, url.Values{"host": {"m.example.com"}})
	if !hasErrFlash(rec) || !strings.Contains(flashText(rec), "web") {
		t.Fatalf("want a catch-all error naming web, got %q", flashText(rec))
	}
}

func TestShareHostOverlapNamesPaths(t *testing.T) {
	s := newShareEnv(t)
	postForm(t, s.h, s.apiURL+"/domains", s.cookie, url.Values{"host": {"m.example.com"}, "paths": {"/api"}})
	// web owns the catch-all; a third row is impossible without a third app, so
	// reuse web's own row: turning its catch-all into /api/v2 overlaps api's /api.
	doms, _ := s.q.ListDomainsByApplication(context.Background(), s.webID)
	rec := postForm(t, s.h, s.webURL+"/domains/"+i64(doms[0].ID)+"/exposure", s.cookie, url.Values{"exposed": {"on"}, "paths": {"/api/v2"}})
	msg := flashText(rec)
	if !hasErrFlash(rec) || !strings.Contains(msg, "/api/v2") || !strings.Contains(msg, "api") {
		t.Fatalf("want an overlap error, got %q", msg)
	}
}

// Review Focus 4: another organization learns nothing about who holds the host.
func TestShareHostFromAnotherOrgIsGeneric(t *testing.T) {
	s := newShareEnv(t)
	ctx := context.Background()
	orgSvc := orgServiceFor(s.q)
	intruder := mkUser(t, s.q, "intruder@k.local")
	o, _ := orgSvc.CreateOrg(ctx, intruder, "Evil")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, _ := s.q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "evil", Image: "nginx", Tag: "alpine", Domain: "evil.example.com", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	_, _ = s.q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: a.ID, Host: "evil.example.com", IsPrimary: true})
	cookie := loginAs(t, s.q, "intruder@k.local")
	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID) + "/domains"
	rec := postForm(t, s.h, target, cookie, url.Values{"host": {"m.example.com"}, "paths": {"/api"}})
	msg := flashText(rec)
	if !hasErrFlash(rec) || strings.Contains(msg, "web") {
		t.Fatalf("want a generic host-in-use error, got %q", msg)
	}
}

func TestCreateAppStillRefusesATakenHost(t *testing.T) {
	s := newShareEnv(t)
	rec := postForm(t, s.h, s.envBase+"/apps", s.cookie, url.Values{
		"name": {"third"}, "image": {"nginx"}, "port": {"80"}, "domain": {"m.example.com"},
	})
	if !hasErrFlash(rec) {
		t.Fatal("a shared host must not become another app's primary domain")
	}
}

// Review Focus 5: TLS on a shared host re-labels every app on it.
func TestToggleTLSOnSharedHostRelabelsEveryApp(t *testing.T) {
	eng := &labelEngine{}
	h, q, orgSvc := newServerWithLabelEngine(t, eng)
	ctx := context.Background()
	owner := mkUser(t, q, "tls-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	envBase := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID)
	mk := func(name, host string) int64 {
		a, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
			EnvironmentID: e.ID, Name: name, Image: "nginx", Tag: "alpine", Domain: host, Port: 80,
			SourceType: "image", DockerfilePath: "Dockerfile",
		})
		_, _ = q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: a.ID, Host: host, IsPrimary: true, Exposed: true})
		return a.ID
	}
	webID, apiID := mk("web", "m.example.com"), mk("api", "api.primary.example.com")
	cookie := loginAs(t, q, "tls-owner@k.local")
	apiURL := envBase + "/apps/" + i64(apiID)
	postForm(t, h, apiURL+"/domains", cookie, url.Values{"host": {"m.example.com"}, "exposed": {"on"}, "paths": {"/api"}})

	var shared db.Domain
	doms, _ := q.ListDomainsByApplication(ctx, apiID)
	for _, d := range doms {
		if d.Host == "m.example.com" {
			shared = d
		}
	}
	rec := postForm(t, h, apiURL+"/domains/"+i64(shared.ID)+"/tls", cookie, url.Values{"tls": {"1"}})
	if hasErrFlash(rec) {
		t.Fatalf("toggle refused: %q", flashText(rec))
	}
	for _, id := range []int64{webID, apiID} {
		for _, d := range mustDomains(t, q, id) {
			if d.Host == "m.example.com" && !d.Tls {
				t.Errorf("app %d: row on the shared host still plain", id)
			}
		}
		l := eng.labelsOf("krill-" + i64(id))
		found := false
		for k, v := range l {
			if strings.HasSuffix(k, ".tls.certresolver") && v == "le" {
				found = true
			}
		}
		if !found {
			t.Errorf("app %d: labels not re-applied with TLS: %v", id, l)
		}
	}
}

func mustDomains(t *testing.T, q *db.Queries, appID int64) []db.Domain {
	t.Helper()
	d, err := q.ListDomainsByApplication(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func TestDomainsTabShowsSharers(t *testing.T) {
	s := newShareEnv(t)
	postForm(t, s.h, s.apiURL+"/domains", s.cookie, url.Values{"host": {"m.example.com"}, "exposed": {"on"}, "paths": {"/api"}})
	rec := getWithCookie(t, s.h, s.webURL+"?tab=domains", s.cookie)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("domains page: status %d", rec.Code)
	}
	if !strings.Contains(body, "HTTPS will switch for every app on m.example.com: web, api") {
		t.Fatal("host-wide HTTPS confirmation missing")
	}
	if !strings.Contains(body, "Shared host with") || !strings.Contains(body, s.apiURL) || !strings.Contains(body, "/api") {
		t.Fatalf("web's domains tab does not show api on the shared host")
	}
}

func TestShareHostFromAnotherEnvironmentIsGeneric(t *testing.T) {
	s := newShareEnv(t)
	ctx := context.Background()
	web, err := s.q.GetApplication(ctx, s.webID)
	if err != nil {
		t.Fatal(err)
	}
	prod, err := s.q.GetEnvironment(ctx, web.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := orgServiceFor(s.q).CreateEnvironment(ctx, prod.ProjectID, "staging")
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: stage.ID, Name: "stage", Domain: "stage.example.com", Image: "nginx",
		Tag: "alpine", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	target := strings.Replace(s.apiURL, "/environments/"+i64(prod.ID)+"/", "/environments/"+i64(stage.ID)+"/", 1)
	target = strings.TrimSuffix(target, i64(s.apiID)) + i64(app.ID) + "/domains"
	rec := postForm(t, s.h, target, s.cookie, url.Values{"host": {"m.example.com"}, "paths": {"/stage"}})
	if !hasErrFlash(rec) || flashText(rec) != "host already in use" {
		t.Fatalf("foreign environment: %d %q", rec.Code, flashText(rec))
	}
	domains, err := s.q.ListDomainsByApplication(ctx, app.ID)
	if err != nil || len(domains) != 0 {
		t.Fatalf("foreign host persisted: %v %v", domains, err)
	}
}
