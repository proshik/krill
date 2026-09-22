package server_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

const panelHost = "krill.example.com"

type panelEnv struct {
	h      http.Handler
	q      *db.Queries
	orgSvc *org.Service
	tokens panel.Tokens
	fw     *hostFirewall
	cert   error // what the certificate check returns
}

// newPanelServer wires the panel gateway the way main does: a stored secret,
// derived tokens and an upstream from the advertise address.
func newPanelServer(t *testing.T, advertise string) *panelEnv {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	secret, err := panel.EnsureSecret(context.Background(), q)
	if err != nil {
		t.Fatalf("ensure secret: %v", err)
	}
	env := &panelEnv{q: q, orgSvc: org.NewService(q), tokens: panel.DeriveTokens(secret), fw: &hostFirewall{}}
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", ListenAddr: ":8080", AdvertiseAddr: advertise}
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), env.orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	srv.SetControlPlaneFirewall(env.fw)
	srv.SetPanelCertCheck(func(context.Context, string) error { return env.cert })
	upstream, uerr := panel.Upstream(advertise, cfg.ListenAddr)
	srv.SetPanelGateway(env.tokens, upstream, uerr)
	env.h = srv.Router()
	return env
}

// viaDomain dresses req up as the gateway would deliver it from the panel
// domain over HTTPS.
func (e *panelEnv) viaDomain(req *http.Request, clientIP string) {
	req.Host = panelHost
	req.Header.Set(panel.ForwardedHeader, e.tokens.Forwarded)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("Origin", "https://"+panelHost)
}

func (e *panelEnv) post(t *testing.T, target string, cookie *http.Cookie, form url.Values, onDomain bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	if onDomain {
		e.viaDomain(req, "203.0.113.7")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *panelEnv) row(t *testing.T) db.PanelGateway {
	t.Helper()
	row, err := e.q.GetPanelGateway(context.Background())
	if err != nil {
		t.Fatalf("read panel row: %v", err)
	}
	return row
}

func (e *panelEnv) config(t *testing.T, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, panel.ProviderPath, nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// The gateway's poll is the only thing that may read the panel's routes.
func TestGatewayConfigAuth(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	if rec := e.config(t, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("no token: want 404, got %d", rec.Code)
	}
	if rec := e.config(t, panel.ProviderHeader, "wrong"); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong token: want 404, got %d", rec.Code)
	}
	// The forwarded token is on every request through the panel domain; it
	// must not open the configuration, which contains it.
	if rec := e.config(t, panel.ForwardedHeader, e.tokens.Forwarded); rec.Code != http.StatusNotFound {
		t.Fatalf("forwarded token: want 404, got %d", rec.Code)
	}
	rec := e.config(t, panel.ProviderHeader, e.tokens.Provider)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("off: want 200 {}, got %d %q", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, panel.ProviderPath, nil)
	req.Header.Set(panel.ProviderHeader, e.tokens.Provider)
	req.Header.Set(panel.ForwardedHeader, e.tokens.Forwarded)
	both := httptest.NewRecorder()
	e.h.ServeHTTP(both, req)
	if both.Code != http.StatusNotFound {
		t.Fatalf("a request proxied through the gateway must not read the config, got %d", both.Code)
	}
}

// Enabling the domain routes it at once but changes nothing else: the direct
// address keeps signing in with ordinary cookies, and only a request through
// the domain over HTTPS with a trusted certificate confirms it.
func TestPanelDomainPendingThenConfirm(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-admin@k.local", "OrgPanel")

	for _, bad := range []string{"", "198.51.100.10", "https://krill.example.com", "krill.example.com:443"} {
		if rec := e.post(t, base+"/panel-domain", cookie, url.Values{"host": {bad}}, false); !hasErrFlash(rec) {
			t.Fatalf("host %q must be rejected, got %q", bad, flashCookieValue(rec))
		}
	}
	rec := e.post(t, base+"/panel-domain", cookie, url.Values{"host": {"Krill.Example.com"}}, false)
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("set domain: %d %q", rec.Code, flashCookieValue(rec))
	}
	if row := e.row(t); row.Host != panelHost || row.State != panel.StatePending {
		t.Fatalf("want pending %s, got %+v", panelHost, row)
	}

	cfg := e.config(t, panel.ProviderHeader, e.tokens.Provider)
	var parsed map[string]any
	if err := json.Unmarshal(cfg.Body.Bytes(), &parsed); err != nil || !strings.Contains(cfg.Body.String(), "Host(`krill.example.com`)") {
		t.Fatalf("pending domain must be routed: %v %s", err, cfg.Body.String())
	}
	if !strings.Contains(cfg.Body.String(), "http://198.51.100.10:8080") {
		t.Fatalf("the route must point at the advertise address: %s", cfg.Body.String())
	}

	// Confirming from the direct address proves nothing.
	if rec := e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, false); !hasErrFlash(rec) {
		t.Fatalf("direct confirm must be refused, got %q", flashCookieValue(rec))
	}
	// Nor does a forged header without the token.
	req := httptest.NewRequest(http.MethodPost, base+"/panel-domain/confirm", nil)
	req.AddCookie(cookie)
	req.Host = panelHost
	req.Header.Set(panel.ForwardedHeader, "guess")
	req.Header.Set("X-Forwarded-Proto", "https")
	forged := httptest.NewRecorder()
	e.h.ServeHTTP(forged, req)
	if !hasErrFlash(forged) {
		t.Fatalf("a forged gateway header must be refused, got %q", flashCookieValue(forged))
	}
	// Through the domain, but the certificate is still Traefik's default.
	e.cert = errors.New("x509: certificate signed by unknown authority")
	if rec := e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, true); !hasErrFlash(rec) {
		t.Fatalf("an untrusted certificate must block confirmation, got %q", flashCookieValue(rec))
	}
	if e.row(t).State != panel.StatePending {
		t.Fatal("refused confirmations must leave the domain pending")
	}
	e.cert = nil
	if rec := e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, true); hasErrFlash(rec) {
		t.Fatalf("confirm through the domain: %q", flashCookieValue(rec))
	}
	if e.row(t).State != panel.StateActive {
		t.Fatal("want active after confirmation")
	}

	if rec := e.post(t, base+"/panel-domain/disable", cookie, url.Values{}, false); hasErrFlash(rec) {
		t.Fatalf("disable: %q", flashCookieValue(rec))
	}
	if row := e.row(t); row.State != panel.StateOff || row.Host != "" {
		t.Fatalf("want off, got %+v", row)
	}
	if rec := e.config(t, panel.ProviderHeader, e.tokens.Provider); strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("a disabled domain must be unrouted, got %s", rec.Body.String())
	}
}

// Without an address the gateway can reach Krill at, the domain cannot be set
// and the page says why.
func TestPanelDomainUnavailableWithoutAdvertise(t *testing.T) {
	e := newPanelServer(t, "")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-noadv@k.local", "OrgPanelNoAdv")
	if rec := e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false); !hasErrFlash(rec) {
		t.Fatalf("want err flash, got %q", flashCookieValue(rec))
	}
	if e.row(t).State != panel.StateOff {
		t.Fatal("nothing must be saved")
	}
	req := httptest.NewRequest(http.MethodGet, base+"/panel-domain", nil)
	req.AddCookie(cookie)
	page := httptest.NewRecorder()
	e.h.ServeHTTP(page, req)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "KRILL_ADVERTISE_ADDR") {
		t.Fatalf("page must explain the missing advertise address: %d", page.Code)
	}
}

// The panel page is global infrastructure: an ordinary org owner gets a 404.
func TestPanelDomainRequiresInstanceAdmin(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	ownerID := mkUser(t, e.q, "panel-owner@k.local")
	o, err := e.orgSvc.CreateOrg(context.Background(), ownerID, "OrgPanelOwner")
	if err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, e.q, "panel-owner@k.local")
	if rec := e.post(t, "/orgs/"+i64(o.ID)+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

// An app must not be able to claim the panel's domain, and the panel must not
// take a domain an app already has.
func TestPanelDomainAppCollision(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	base, cookie, orgID := nodesOrg(t, e.q, e.orgSvc, "panel-coll@k.local", "OrgPanelColl")
	p, _ := e.orgSvc.CreateProject(context.Background(), orgID, "Proj", "")
	env, _ := e.orgSvc.CreateEnvironment(context.Background(), p.ID, "production")
	appsURL := base + "/projects/" + i64(p.ID) + "/environments/" + i64(env.ID) + "/apps"

	if rec := e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false); hasErrFlash(rec) {
		t.Fatalf("set domain: %q", flashCookieValue(rec))
	}
	rec := postForm(t, e.h, appsURL, cookie, url.Values{
		"name": {"web"}, "source_type": {"image"}, "image": {"nginx:alpine"}, "port": {"80"}, "domain": {panelHost},
	})
	if !hasErrFlash(rec) {
		t.Fatalf("an app must not take the panel domain, got %d %q", rec.Code, flashCookieValue(rec))
	}

	if rec := e.post(t, base+"/panel-domain/disable", cookie, url.Values{}, false); hasErrFlash(rec) {
		t.Fatalf("disable: %q", flashCookieValue(rec))
	}
	rec = postForm(t, e.h, appsURL, cookie, url.Values{
		"name": {"web"}, "source_type": {"image"}, "image": {"nginx:alpine"}, "port": {"80"}, "domain": {panelHost},
	})
	if hasErrFlash(rec) {
		t.Fatalf("create app on a free domain: %q", flashCookieValue(rec))
	}
	if rec := e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false); !hasErrFlash(rec) {
		t.Fatalf("the panel must not take an app's domain, got %q", flashCookieValue(rec))
	}
}

// Cookies are Secure exactly for requests that reached the gateway over HTTPS.
// This per-request decision replaced a global KRILL_COOKIE_SECURE flip; if it
// broke, nothing would fail visibly — sessions would just travel unprotected.
func TestPanelCookiesSecureOnlyViaGateway(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	mkUser(t, e.q, "panel-cookie@k.local")
	type path struct {
		name    string
		forward string // forwarded token; "" = a direct request
		proto   string
		secure  bool
	}
	paths := []path{
		{"direct to the UI port", "", "", false},
		{"direct, claiming https", "", "https", false},
		{"through the gateway over HTTPS", e.tokens.Forwarded, "https", true},
		{"through the gateway over plain HTTP", e.tokens.Forwarded, "http", false},
		{"forged gateway token", "forged", "https", false},
	}
	dress := func(req *http.Request, p path) {
		req.Host = panelHost
		if p.forward != "" {
			req.Header.Set(panel.ForwardedHeader, p.forward)
		}
		if p.proto != "" {
			req.Header.Set("X-Forwarded-Proto", p.proto)
		}
	}
	find := func(rec *httptest.ResponseRecorder, name string) *http.Cookie {
		for _, c := range rec.Result().Cookies() {
			if c.Name == name {
				return c
			}
		}
		return nil
	}
	for _, p := range paths {
		// The session cookie, set by a successful sign-in.
		form := url.Values{"email": {"panel-cookie@k.local"}, "password": {"pw"}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		dress(req, p)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		session := find(rec, auth.CookieName)
		if session == nil {
			t.Fatalf("%s: no session cookie (status %d)", p.name, rec.Code)
		}
		if session.Secure != p.secure {
			t.Errorf("%s: session cookie Secure = %v, want %v", p.name, session.Secure, p.secure)
		}
		if !session.HttpOnly {
			t.Errorf("%s: session cookie must stay HttpOnly", p.name)
		}

		// A flash cookie, set by any form post with the signed-in session.
		base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-cookie-"+strings.ReplaceAll(p.name, " ", "-")+"@k.local", "OrgCookie "+p.name)
		freq := httptest.NewRequest(http.MethodPost, base+"/panel-domain", strings.NewReader(url.Values{"host": {"not a host"}}.Encode()))
		freq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		freq.AddCookie(cookie)
		dress(freq, p)
		frec := httptest.NewRecorder()
		e.h.ServeHTTP(frec, freq)
		flash := find(frec, "krill_flash")
		if flash == nil {
			t.Fatalf("%s: no flash cookie (status %d)", p.name, frec.Code)
		}
		if flash.Secure != p.secure {
			t.Errorf("%s: flash cookie Secure = %v, want %v", p.name, flash.Secure, p.secure)
		}
	}
}

// KRILL_COOKIE_SECURE=true still forces Secure on a direct request — the
// setting for a TLS proxy of the operator's own — and the sign-in page warns
// that it cannot work over plain HTTP.
func TestCookieSecureConfigForcesSecure(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", ListenAddr: ":8080", CookieSecure: true}
	hub := deploy.NewLogHub()
	h := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net")).Router()
	mkUser(t, q, "forced-secure@k.local")

	form := url.Values{"email": {"forced-secure@k.local"}, "password": {"pw"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var secure bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			secure = c.Secure
		}
	}
	if !secure {
		t.Fatal("KRILL_COOKIE_SECURE=true must force Secure")
	}
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", nil))
	if !strings.Contains(page.Body.String(), "KRILL_COOKIE_SECURE") {
		t.Fatal("the sign-in page must warn that secure cookies cannot work over plain HTTP")
	}
}

// HSTS is sent only on the active panel domain over HTTPS, only for its host,
// is off (max-age=0) until set, cannot be set before the domain is confirmed,
// never reaches subdomains, and is reset whenever the domain changes.
func TestPanelHSTS(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-hsts@k.local", "OrgPanelHSTS")
	hsts := func(host, forward, proto string) (string, bool) {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.Host = host
		if forward != "" {
			req.Header.Set(panel.ForwardedHeader, forward)
		}
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		v, ok := rec.Header()[panel.HSTSHeader]
		if !ok {
			return "", false
		}
		return v[0], true
	}
	onDomain := func() (string, bool) { return hsts(panelHost, e.tokens.Forwarded, "https") }

	if _, ok := onDomain(); ok {
		t.Fatal("no domain: no header")
	}
	e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false)
	if _, ok := onDomain(); ok {
		t.Fatal("a pending domain is unproven and must not get HSTS")
	}
	if rec := e.post(t, base+"/panel-domain/hsts", cookie, url.Values{"max_age": {"300"}}, true); !hasErrFlash(rec) {
		t.Fatalf("HSTS before confirmation must be refused, got %q", flashCookieValue(rec))
	}
	if e.row(t).HstsMaxAge != 0 {
		t.Fatal("a refused HSTS change must not be stored")
	}
	e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, true)

	if v, ok := onDomain(); !ok || v != "max-age=0" {
		t.Fatalf("active with HSTS off must send max-age=0, got %q %v", v, ok)
	}
	for _, bad := range []string{"123", "-1", "63072000", "abc", ""} {
		if rec := e.post(t, base+"/panel-domain/hsts", cookie, url.Values{"max_age": {bad}}, true); !hasErrFlash(rec) {
			t.Fatalf("max_age %q must be rejected", bad)
		}
	}
	if rec := e.post(t, base+"/panel-domain/hsts", cookie, url.Values{"max_age": {"86400"}}, false); hasErrFlash(rec) {
		t.Fatalf("set HSTS: %q", flashCookieValue(rec))
	}
	if v, _ := onDomain(); v != "max-age=86400" {
		t.Fatalf("want max-age=86400, got %q", v)
	}
	if v, _ := onDomain(); strings.Contains(v, "includeSubDomains") || strings.Contains(v, "preload") {
		t.Fatalf("HSTS must not reach beyond the panel host: %q", v)
	}
	// Never on the direct address, over plain HTTP, with a forged token, or for
	// another host the gateway might hand over.
	for name, got := range map[string]func() (string, bool){
		"direct":        func() (string, bool) { return hsts("198.51.100.10:8080", "", "") },
		"direct claims": func() (string, bool) { return hsts(panelHost, "", "https") },
		"gateway http":  func() (string, bool) { return hsts(panelHost, e.tokens.Forwarded, "http") },
		"forged":        func() (string, bool) { return hsts(panelHost, "forged", "https") },
		"other host":    func() (string, bool) { return hsts("pacer.example.com", e.tokens.Forwarded, "https") },
	} {
		if v, ok := got(); ok {
			t.Errorf("%s: must not get HSTS, got %q", name, v)
		}
	}

	// Moving the domain puts it back to pending with HSTS off.
	e.post(t, base+"/panel-domain", cookie, url.Values{"host": {"panel.example.com"}}, false)
	if row := e.row(t); row.State != panel.StatePending || row.HstsMaxAge != 0 {
		t.Fatalf("a new domain must start without HSTS, got %+v", row)
	}
	e.post(t, base+"/panel-domain/disable", cookie, url.Values{}, false)
	if e.row(t).HstsMaxAge != 0 {
		t.Fatal("a removed domain must not keep HSTS")
	}
}

// Through the gateway every request shares Traefik's source address; the login
// limiter has to key on the client address Traefik appended instead. Straight
// at the UI port, the same header is ignored.
func TestPanelLoginRateLimitKeysOnGatewayClient(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	attempt := func(onDomain bool, xff string) int {
		form := url.Values{"email": {"nobody@k.local"}, "password": {"x"}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "172.18.0.5:40000"
		if onDomain {
			e.viaDomain(req, xff)
		} else {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 10; i++ {
		attempt(true, "198.51.100.1")
	}
	if code := attempt(true, "198.51.100.1"); code != http.StatusTooManyRequests {
		t.Fatalf("the 11th attempt from one client must be limited, got %d", code)
	}
	if code := attempt(true, "198.51.100.2"); code == http.StatusTooManyRequests {
		t.Fatal("another client behind the gateway must have its own bucket")
	}
	// Direct requests from the same source share one bucket, whatever they claim.
	for i := 0; i < 10; i++ {
		attempt(false, "192.0.2."+i64(int64(i)))
	}
	if code := attempt(false, "192.0.2.99"); code != http.StatusTooManyRequests {
		t.Fatalf("a direct client must not mint buckets with X-Forwarded-For, got %d", code)
	}
}

// An operator on the domain cannot save an allowlist that excludes them.
func TestPanelAllowlistRefusesSelfLockout(t *testing.T) {
	e := newPanelServer(t, "198.51.100.10")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-allow@k.local", "OrgPanelAllow")
	e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false)

	if rec := e.post(t, base+"/panel-domain/allowed-ips", cookie, url.Values{"allowed_ips": {"10.0.0.0/8"}}, true); !hasErrFlash(rec) {
		t.Fatalf("a list without the operator's address must be refused, got %q", flashCookieValue(rec))
	}
	if rec := e.post(t, base+"/panel-domain/allowed-ips", cookie, url.Values{"allowed_ips": {"203.0.113.0/24\n10.0.0.0/8"}}, true); hasErrFlash(rec) {
		t.Fatalf("a list with the operator's address: %q", flashCookieValue(rec))
	}
	if got := e.row(t).AllowedIps; got != "203.0.113.0/24\n10.0.0.0/8" {
		t.Fatalf("stored %q", got)
	}
	if rec := e.post(t, base+"/panel-domain/allowed-ips", cookie, url.Values{"allowed_ips": {"not-an-ip"}}, true); !hasErrFlash(rec) {
		t.Fatal("invalid entries must be rejected")
	}
	if rec := e.config(t, panel.ProviderHeader, e.tokens.Provider); !strings.Contains(rec.Body.String(), "203.0.113.0/24") {
		t.Fatalf("the allowlist must reach the gateway: %s", rec.Body.String())
	}
}

// The full direct-port protocol: close from the domain under lockdown, keep the
// dead-man switch armed, confirm through the domain, refuse anything that would
// strand the operator, then reopen.
func TestPanelDirectPortCloseConfirmOpen(t *testing.T) {
	e := newPanelServer(t, "localhost")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-direct@k.local", "OrgPanelDirect")
	e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false)

	// Not active yet.
	if rec := e.post(t, base+"/panel-domain/direct-port/close", cookie, url.Values{}, true); !hasErrFlash(rec) || e.fw.locked {
		t.Fatalf("closing before the domain is confirmed must be refused, got %q", flashCookieValue(rec))
	}
	e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, true)
	if e.row(t).State != panel.StateActive {
		t.Fatal("setup: domain must be active")
	}
	// No lockdown to narrow.
	if rec := e.post(t, base+"/panel-domain/direct-port/close", cookie, url.Values{}, true); !hasErrFlash(rec) || len(e.fw.rulesets) != 0 {
		t.Fatalf("closing without a lockdown must be refused, got %q", flashCookieValue(rec))
	}
	// Lockdown from the direct address, as today.
	if rec := postForm(t, e.h, base+"/firewall/lockdown", cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("lockdown: %q", flashCookieValue(rec))
	}
	postForm(t, e.h, base+"/firewall/confirm", cookie, url.Values{})
	// From the direct address: refused, it would be confirmed from nowhere.
	if rec := e.post(t, base+"/panel-domain/direct-port/close", cookie, url.Values{}, false); !hasErrFlash(rec) || len(e.fw.rulesets) != 1 {
		t.Fatalf("closing from the direct address must be refused, got %q", flashCookieValue(rec))
	}

	rec := e.post(t, base+"/panel-domain/direct-port/close", cookie, url.Values{}, true)
	if hasErrFlash(rec) || rec.Header().Get("Connection") != "close" {
		t.Fatalf("close: %q connection=%q", flashCookieValue(rec), rec.Header().Get("Connection"))
	}
	last := e.fw.rulesets[len(e.fw.rulesets)-1]
	if !strings.Contains(last, "tcp dport { 80, 443 } accept") || !strings.Contains(last, `iifname "docker_gwbridge" tcp dport { 8080 } accept`) {
		t.Fatalf("the UI port must be gateway-only:\n%s", last)
	}
	if !e.fw.pending {
		t.Fatal("the dead-man switch must stay armed until the operator confirms")
	}
	if row := e.row(t); row.DirectPortClosed || !row.DirectPortClosePending {
		t.Fatalf("want a pending close, got %+v", row)
	}
	// The domain cannot be removed or moved while it is the way in.
	if rec := e.post(t, base+"/panel-domain/disable", cookie, url.Values{}, true); !hasErrFlash(rec) {
		t.Fatal("disabling with the port closing must be refused")
	}
	if rec := e.post(t, base+"/panel-domain", cookie, url.Values{"host": {"other.example.com"}}, true); !hasErrFlash(rec) {
		t.Fatal("moving the domain with the port closing must be refused")
	}

	confirms := e.fw.confirms
	if rec := e.post(t, base+"/panel-domain/direct-port/confirm", cookie, url.Values{}, false); !hasErrFlash(rec) || e.fw.confirms != confirms {
		t.Fatal("confirmation from the direct address must be refused")
	}
	if rec := e.post(t, base+"/panel-domain/direct-port/confirm", cookie, url.Values{}, true); hasErrFlash(rec) || e.fw.pending {
		t.Fatalf("confirm through the domain: %q pending=%v", flashCookieValue(rec), e.fw.pending)
	}
	if row := e.row(t); !row.DirectPortClosed || row.DirectPortClosePending {
		t.Fatalf("want closed, got %+v", row)
	}

	// A later lockdown keeps the port closed — and must run from the domain.
	if rec := postForm(t, e.h, base+"/firewall/lockdown", cookie, url.Values{}); !hasErrFlash(rec) {
		t.Fatal("a lockdown from the direct address would be unconfirmable and must be refused")
	}
	if rec := e.post(t, base+"/firewall/lockdown", cookie, url.Values{}, true); hasErrFlash(rec) {
		t.Fatalf("lockdown from the domain: %q", flashCookieValue(rec))
	}
	if last := e.fw.rulesets[len(e.fw.rulesets)-1]; strings.Contains(last, "80, 443, 8080") {
		t.Fatalf("a re-lockdown must keep the port closed:\n%s", last)
	}
	e.post(t, base+"/firewall/confirm", cookie, url.Values{}, true)

	if rec := e.post(t, base+"/panel-domain/direct-port/open", cookie, url.Values{}, true); hasErrFlash(rec) {
		t.Fatalf("open: %q", flashCookieValue(rec))
	}
	if last := e.fw.rulesets[len(e.fw.rulesets)-1]; !strings.Contains(last, "tcp dport { 80, 443, 8080 } accept") || e.fw.pending {
		t.Fatalf("reopening must restore the public port and confirm at once: pending=%v\n%s", e.fw.pending, last)
	}
	if row := e.row(t); row.DirectPortClosed || row.DirectPortClosePending {
		t.Fatalf("want open, got %+v", row)
	}
}

// A close that nobody confirmed reverts on the host; the page notices and the
// stored state follows, so the domain can be managed again.
func TestPanelDirectPortRevertDetected(t *testing.T) {
	e := newPanelServer(t, "localhost")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-revert@k.local", "OrgPanelRevert")
	e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false)
	e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, true)
	postForm(t, e.h, base+"/firewall/lockdown", cookie, url.Values{})
	postForm(t, e.h, base+"/firewall/confirm", cookie, url.Values{})
	e.post(t, base+"/panel-domain/direct-port/close", cookie, url.Values{}, true)
	if !e.row(t).DirectPortClosePending {
		t.Fatal("setup: want a pending close")
	}

	e.fw.pending = false // the dead-man switch fired

	req := httptest.NewRequest(http.MethodGet, base+"/panel-domain", nil)
	req.AddCookie(cookie)
	page := httptest.NewRecorder()
	e.h.ServeHTTP(page, req)
	if page.Code != http.StatusOK {
		t.Fatalf("page: %d", page.Code)
	}
	if row := e.row(t); row.DirectPortClosed || row.DirectPortClosePending {
		t.Fatalf("a reverted close must be cleared, got %+v", row)
	}
	if rec := e.post(t, base+"/panel-domain/disable", cookie, url.Values{}, false); hasErrFlash(rec) {
		t.Fatalf("after the revert the domain can be removed again: %q", flashCookieValue(rec))
	}
}

// Opening the firewall reopens the port, and the stored state has to say so.
func TestFirewallOpenResetsPanelDirectPort(t *testing.T) {
	e := newPanelServer(t, "localhost")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "panel-fwopen@k.local", "OrgPanelFWOpen")
	e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false)
	e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, true)
	postForm(t, e.h, base+"/firewall/lockdown", cookie, url.Values{})
	postForm(t, e.h, base+"/firewall/confirm", cookie, url.Values{})
	e.post(t, base+"/panel-domain/direct-port/close", cookie, url.Values{}, true)
	e.post(t, base+"/panel-domain/direct-port/confirm", cookie, url.Values{}, true)
	if !e.row(t).DirectPortClosed {
		t.Fatal("setup: want closed")
	}
	e.post(t, base+"/firewall/open", cookie, url.Values{}, true)
	if row := e.row(t); row.DirectPortClosed {
		t.Fatalf("an open firewall leaves nothing closed, got %+v", row)
	}
}
