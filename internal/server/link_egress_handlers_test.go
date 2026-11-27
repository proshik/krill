package server_test

import (
	"context"
	"html"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/link"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/web/i18n"
)

// gateInstaller is an updater whose rollback binary is b.
type gateInstaller struct {
	*fakeUpdateInstaller
	b selfupdate.RollbackBinary
}

func (g gateInstaller) RollbackBinary(context.Context) selfupdate.RollbackBinary { return g.b }

func withRollback(b selfupdate.RollbackBinary) func(*server.Server, *linkEnv) {
	return func(srv *server.Server, _ *linkEnv) {
		srv.SetSelfUpdate(&fakeUpdateChecker{}, gateInstaller{fakeUpdateInstaller: &fakeUpdateInstaller{}, b: b})
	}
}

// egressEngine satisfies what link.NewEgress needs; nothing here reconciles.
type egressEngine struct{ docker.Engine }

func (egressEngine) PrefixNetworks(context.Context, string) ([]docker.PrefixNetwork, error) {
	return nil, nil
}
func (egressEngine) ConfigEnsure(context.Context, string, []byte, map[string]string) error { return nil }
func (egressEngine) SecretEnsure(context.Context, string, []byte, map[string]string) error { return nil }
func (egressEngine) PruneObjects(context.Context, string, string, []string) error         { return nil }

func withEgress(t *testing.T) func(*server.Server, *linkEnv) {
	return func(_ *server.Server, e *linkEnv) {
		if _, err := link.NewEgress(e.link, egressEngine{}, e.secret); err != nil {
			t.Fatal(err)
		}
	}
}

var (
	gateOpen  = selfupdate.RollbackBinary{Kind: selfupdate.RollbackRelease, Version: link.CoreRelease}
	gateOlder = selfupdate.RollbackBinary{Kind: selfupdate.RollbackRelease, Version: "v0.16.3"}
)

// egressOrg is an operator's organization with one app, the link a client.
func egressOrg(t *testing.T, e *linkEnv, email string) (base string, cookie *http.Cookie, orgID, appID int64) {
	t.Helper()
	ctx := context.Background()
	base, cookie, orgID = nodesOrg(t, e.q, e.orgSvc, email, "Org-"+email)
	p, err := e.orgSvc.CreateProject(ctx, orgID, "bots", "")
	if err != nil {
		t.Fatal(err)
	}
	env, err := e.orgSvc.CreateEnvironment(ctx, p.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: env.ID, Name: "notifier", Image: "nginx", Tag: "alpine",
		Domain: "notifier.example.com", Port: 80, SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := postForm(t, e.h, base+"/link/role", cookie, url.Values{"role": {"client"}}); hasErrFlash(rec) {
		t.Fatalf("set client: %q", flashCookieValue(rec))
	}
	return base, cookie, orgID, a.ID
}

func egressAppURL(base string, e *linkEnv, appID int64) string {
	a, _ := e.q.GetApplication(context.Background(), appID)
	env, _ := e.q.GetEnvironment(context.Background(), a.EnvironmentID)
	return base + "/projects/" + i64(env.ProjectID) + "/environments/" + i64(env.ID) + "/apps/" + i64(appID)
}

func grants(t *testing.T, e *linkEnv) map[int64]bool {
	t.Helper()
	g, err := e.link.Grants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func flashHasText(rec *httptest.ResponseRecorder, want ...string) bool {
	v, _ := url.QueryUnescape(flashCookieValue(rec))
	for _, w := range want {
		if !strings.Contains(v, w) {
			return false
		}
	}
	return true
}

// While the rollback binary is older than the link core release, cannot be
// read, or the updater is not wired, granting an organization is refused with
// the reason and nothing is written; with none or at least the core release
// it is granted.
func TestLinkGrantRollbackGate(t *testing.T) {
	en := i18n.WithLocale(context.Background(), "en")
	cases := []struct {
		name    string
		opts    []func(*server.Server, *linkEnv)
		granted bool
		flash   []string
	}{
		{"older than the core", []func(*server.Server, *linkEnv){withRollback(gateOlder)}, false, []string{"v0.16.3", link.CoreRelease}},
		{"unknown", []func(*server.Server, *linkEnv){withRollback(selfupdate.RollbackBinary{Kind: selfupdate.RollbackUnknown})}, false,
			[]string{i18n.Tf(en, "link.gate.unknown", link.CoreRelease)}},
		{"updater not wired", nil, false, []string{i18n.Tf(en, "link.gate.unknown", link.CoreRelease)}},
		{"no krill.prev", []func(*server.Server, *linkEnv){withRollback(selfupdate.RollbackBinary{Kind: selfupdate.RollbackAbsent})}, true, nil},
		{"the core release", []func(*server.Server, *linkEnv){withRollback(gateOpen)}, true, nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newLinkServer(t, "198.51.100.10", tc.opts...)
			base, cookie, orgID, _ := egressOrg(t, e, "gate"+i64(int64(i))+"@k.local")
			page := getPage(t, e.h, base+"/link", cookie, nil)
			if shown := strings.Contains(page.Body.String(), "(krill.prev)"); shown == tc.granted {
				t.Fatalf("page gives the gate's reason = %v, want %v", shown, !tc.granted)
			}
			rec := postForm(t, e.h, base+"/link/grants", cookie, url.Values{"org_id": {i64(orgID)}, "enabled": {"1"}})
			if got := grants(t, e)[orgID]; got != tc.granted {
				t.Fatalf("granted = %v, want %v (flash %q)", got, tc.granted, flashCookieValue(rec))
			}
			if !tc.granted && (!hasErrFlash(rec) || !flashHasText(rec, tc.flash...)) {
				t.Fatalf("flash %q, want %v", flashCookieValue(rec), tc.flash)
			}
		})
	}
}

// A revoke never waits for the gate.
func TestLinkRevokeIgnoresTheGate(t *testing.T) {
	e := newLinkServer(t, "198.51.100.10", withRollback(gateOlder))
	base, cookie, orgID, _ := egressOrg(t, e, "revoke@k.local")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO link_org_grants (org_id) VALUES ($1)`, orgID); err != nil {
		t.Fatal(err)
	}
	postForm(t, e.h, base+"/link/grants", cookie, url.Values{"org_id": {i64(orgID)}, "enabled": {"0"}})
	if grants(t, e)[orgID] {
		t.Fatal("revoke refused by the gate")
	}
}

func savedDests(t *testing.T, e *linkEnv, appID int64) []link.Destination {
	t.Helper()
	ds, err := e.link.AppDestinations(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// The app's link card exists only while the link is open to its
// organization: no card and a 404 otherwise. A saved destination may be kept
// or dropped though the relay no longer offers it; one the relay never offered
// is refused. A closed gate refuses the save with its reason and writes
// nothing.
func TestAppLinkSelection(t *testing.T) {
	gate := gateOpen
	e := newLinkServer(t, "198.51.100.10", func(srv *server.Server, env *linkEnv) {
		srv.SetSelfUpdate(&fakeUpdateChecker{}, gateFunc{fakeUpdateInstaller: &fakeUpdateInstaller{}, b: &gate})
	})
	ctx := context.Background()
	base, cookie, orgID, appID := egressOrg(t, e, "applink@k.local")
	app := egressAppURL(base, e, appID)

	page := getPage(t, e.h, app+"?tab=settings", cookie, nil)
	if strings.Contains(page.Body.String(), `id="s-link"`) {
		t.Fatal("link card without a grant")
	}
	if rec := postForm(t, e.h, app+"/link", cookie, url.Values{"dest": {"api.telegram.org:443"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("POST without a grant: %d, want 404", rec.Code)
	}

	postForm(t, e.h, base+"/link/grants", cookie, url.Values{"org_id": {i64(orgID)}, "enabled": {"1"}})
	if !grants(t, e)[orgID] {
		t.Fatal("grant failed")
	}
	page = getPage(t, e.h, app+"?tab=settings", cookie, nil)
	if !strings.Contains(page.Body.String(), `id="s-link"`) || !strings.Contains(page.Body.String(), i18n.T(i18n.WithLocale(ctx, "en"), "applink.host_note")) {
		t.Fatal("no link card with the host-level note after the grant")
	}

	// The relay sent nothing yet: a new destination is not offered.
	if rec := postForm(t, e.h, app+"/link", cookie, url.Values{"dest": {"api.telegram.org:443"}}); !flashHas(rec, "applink.err.not_offered") {
		t.Fatalf("not offered: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, e.h, app+"/link", cookie, url.Values{"dest": {"203.0.113.7:443"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("an IP destination: %d, want 400", rec.Code)
	}
	if rec := postForm(t, e.h, app+"/link", cookie, url.Values{"dest": {"api.telegram.org:0"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("port 0: %d, want 400", rec.Code)
	}
	if len(savedDests(t, e, appID)) != 0 {
		t.Fatal("a refused selection was written")
	}

	// A saved one (the relay since dropped it) can be kept and removed.
	if _, err := e.pool.Exec(ctx, `INSERT INTO app_link_destinations (application_id, host, port) VALUES ($1, 'api.telegram.org', 443)`, appID); err != nil {
		t.Fatal(err)
	}
	page = getPage(t, e.h, app+"?tab=settings", cookie, nil)
	if !strings.Contains(page.Body.String(), i18n.T(i18n.WithLocale(ctx, "en"), "applink.not_allowed")) {
		t.Fatal("a saved destination the relay does not offer is not marked")
	}

	// Gate closed: the save is refused, nothing changes, and the reason
	// shows in both languages.
	gate = gateOlder
	if rec := postForm(t, e.h, app+"/link", cookie, url.Values{}); !hasErrFlash(rec) || !flashHasText(rec, "v0.16.3", link.CoreRelease) {
		t.Fatalf("gate closed: %q", flashCookieValue(rec))
	}
	if len(savedDests(t, e, appID)) != 1 {
		t.Fatal("written while the gate was closed")
	}
	for _, loc := range []string{"en", "ru"} {
		want := i18n.Tf(i18n.WithLocale(ctx, loc), "link.gate.older", "v0.16.3", link.CoreRelease)
		req := httptest.NewRequest(http.MethodGet, app+"?tab=settings", nil)
		req.AddCookie(cookie)
		req.AddCookie(&http.Cookie{Name: "krill_lang", Value: loc})
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), html.EscapeString(want)) {
			t.Fatalf("%s: the card does not give the gate's reason %q", loc, want)
		}
	}
	gate = gateOpen

	if rec := postForm(t, e.h, app+"/link", cookie, url.Values{"dest": {"api.telegram.org:443"}}); hasErrFlash(rec) {
		t.Fatalf("keep the saved one: %q", flashCookieValue(rec))
	}
	if got := savedDests(t, e, appID); len(got) != 1 || got[0].String() != "api.telegram.org:443" {
		t.Fatalf("saved %v", got)
	}
	if rec := postForm(t, e.h, app+"/link", cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("clear: %q", flashCookieValue(rec))
	}
	if len(savedDests(t, e, appID)) != 0 {
		t.Fatal("not cleared")
	}
}

// gateFunc is gateInstaller with a gate the test changes as it goes.
type gateFunc struct {
	*fakeUpdateInstaller
	b *selfupdate.RollbackBinary
}

func (g gateFunc) RollbackBinary(context.Context) selfupdate.RollbackBinary { return *g.b }

// Another organization's app is a 404, never a 403, and a member is refused
// by the role gate.
func TestAppLinkTenancy(t *testing.T) {
	e := newLinkServer(t, "198.51.100.10", withRollback(gateOpen))
	ctx := context.Background()
	baseA, cookieA, orgA, appA := egressOrg(t, e, "tenant-a@k.local")
	postForm(t, e.h, baseA+"/link/grants", cookieA, url.Values{"org_id": {i64(orgA)}, "enabled": {"1"}})
	appURL := egressAppURL(baseA, e, appA)

	otherID := mkUser(t, e.q, "tenant-b@k.local")
	ob, err := e.orgSvc.CreateOrg(ctx, otherID, "OrgB")
	if err != nil {
		t.Fatal(err)
	}
	cookieB := loginAs(t, e.q, "tenant-b@k.local")
	crossed := strings.Replace(appURL, baseA, "/orgs/"+i64(ob.ID), 1)
	if rec := postForm(t, e.h, crossed+"/link", cookieB, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org POST: %d, want 404", rec.Code)
	}
	if rec := postForm(t, e.h, appURL+"/link", cookieB, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("POST into another org: %d, want 404", rec.Code)
	}

	memberID := mkUser(t, e.q, "tenant-member@k.local")
	if _, err := e.q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgA, UserID: memberID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if rec := postForm(t, e.h, appURL+"/link", loginAs(t, e.q, "tenant-member@k.local"), url.Values{}); rec.Code != http.StatusForbidden {
		t.Fatalf("member POST: %d, want 403", rec.Code)
	}
	// The grant control is the operator's: an org owner who is not one gets 404.
	if rec := postForm(t, e.h, "/orgs/"+i64(ob.ID)+"/link/grants", cookieB, url.Values{"org_id": {i64(ob.ID)}, "enabled": {"1"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("grant by a non-operator: %d, want 404", rec.Code)
	}
}

// The sidecar's configuration answers only its own token, never through the
// gateway, and routes the saved destinations of granted organizations.
func TestEgressConfigEndpoint(t *testing.T) {
	e := newLinkServer(t, "198.51.100.10", withRollback(gateOpen), withEgress(t))
	ctx := context.Background()
	base, cookie, orgID, appID := egressOrg(t, e, "sidecar@k.local")
	get := func(token string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, link.EgressConfigPath, nil)
		for k, v := range header {
			req.Header[k] = v
		}
		if token != "" {
			req.Header.Set(link.EgressHeader, token)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	token := link.EgressToken(e.secret)
	if rec := get("", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := get(e.tokens.Provider, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the panel's token: %d", rec.Code)
	}
	if rec := get(token, http.Header{panel.ForwardedHeader: {e.tokens.Forwarded}}); rec.Code != http.StatusNotFound {
		t.Fatalf("through the gateway: %d", rec.Code)
	}
	if rec := get(token, nil); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("nothing to route: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO app_link_destinations (application_id, host, port) VALUES ($1, 'api.telegram.org', 443)`, appID); err != nil {
		t.Fatal(err)
	}
	if rec := get(token, nil); strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("routed without a grant: %s", rec.Body.String())
	}
	postForm(t, e.h, base+"/link/grants", cookie, url.Values{"org_id": {i64(orgID)}, "enabled": {"1"}})
	rec := get(token, nil)
	var cfg struct {
		TCP struct {
			Routers map[string]struct{ Rule string } `json:"routers"`
		} `json:"tcp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil || cfg.TCP.Routers["egress-p443"].Rule != "HostSNI(`api.telegram.org`)" {
		t.Fatalf("routes %s (%v)", rec.Body.String(), err)
	}
}
