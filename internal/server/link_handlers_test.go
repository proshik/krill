package server_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/link"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
	"github.com/proshik/krill/internal/web/i18n"
)

const linkHost = "link.example.com"

type linkEnv struct {
	pool   *pgxpool.Pool
	h      http.Handler
	q      *db.Queries
	orgSvc *org.Service
	tokens panel.Tokens
	link   *link.Service
	secret string // the gateway's stored secret
}

func freeLinkPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return ":" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// newLinkServer wires the panel gateway and the link the way main does;
// opts adjust the server before its router is built.
func newLinkServer(t *testing.T, advertise string, opts ...func(*server.Server, *linkEnv)) *linkEnv {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	sec, err := panel.EnsureSecret(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", ListenAddr: ":8080",
		AdvertiseAddr: advertise, LinkListenAddr: freeLinkPort(t)}
	env := &linkEnv{pool: pool, q: q, orgSvc: org.NewService(q), tokens: panel.DeriveTokens(sec), secret: sec}
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), env.orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	hosts := hostshare.NewStore(pool)
	srv.SetHostShare(hosts)
	upstream, uerr := panel.Upstream(advertise, cfg.ListenAddr)
	srv.SetPanelGateway(env.tokens, upstream, uerr)
	env.link = link.NewService(pool, hosts, link.Config{AdvertiseAddr: advertise, ListenAddr: cfg.ListenAddr, LinkListenAddr: cfg.LinkListenAddr, Version: "v0.0.0-test"})
	if err := env.link.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.link.Close)
	srv.SetLink(env.link)
	for _, o := range opts {
		o(srv, env)
	}
	env.h = srv.Router()
	return env
}

func (e *linkEnv) gatewayConfig(t *testing.T) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, panel.ProviderPath, nil)
	req.Header.Set(panel.ProviderHeader, e.tokens.Provider)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway config: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func flashHas(rec *httptest.ResponseRecorder, key string) bool {
	v, _ := url.QueryUnescape(flashCookieValue(rec))
	return strings.Contains(v, i18n.T(i18n.WithLocale(context.Background(), "en"), key))
}

// Settings → Link is global infrastructure: an org owner who is not the
// instance operator gets 404 on every route.
func TestLinkRequiresInstanceAdmin(t *testing.T) {
	e := newLinkServer(t, "198.51.100.10")
	ownerID := mkUser(t, e.q, "link-owner@k.local")
	o, err := e.orgSvc.CreateOrg(context.Background(), ownerID, "OrgLinkOwner")
	if err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, e.q, "link-owner@k.local")
	base := "/orgs/" + i64(o.ID) + "/link"
	if rec := getPage(t, e.h, base, cookie, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET: want 404, got %d", rec.Code)
	}
	for _, p := range []string{"/role", "/codes", "/peers/1/remove", "/pair", "/forget"} {
		if rec := postForm(t, e.h, base+p, cookie, url.Values{"role": {"relay"}, "link_host": {linkHost}, "peer": {"msk"}}); rec.Code != http.StatusNotFound {
			t.Fatalf("POST %s: want 404, got %d", p, rec.Code)
		}
	}
	if e.link.Role() != link.RoleOff {
		t.Fatal("a non-operator changed the link role")
	}
}

// The relay role needs what the panel domain needs; without an advertise
// address it is refused with the reason.
func TestLinkRelayRefusedWithoutAdvertise(t *testing.T) {
	e := newLinkServer(t, "")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "link-noadv@k.local", "OrgLinkNoAdv")
	rec := postForm(t, e.h, base+"/link/role", cookie, url.Values{"role": {"relay"}, "link_host": {linkHost}})
	if !flashHas(rec, "link.req.advertise") {
		t.Fatalf("want the advertise reason, got %q", flashCookieValue(rec))
	}
	if e.link.Role() != link.RoleOff {
		t.Fatal("relay role enabled without an advertise address")
	}
}

// A relay publishes its link host as a TCP passthrough route beside the
// panel's HTTP routes, and only while nothing else holds the host.
func TestLinkRelayGatewayConfig(t *testing.T) {
	e := newLinkServer(t, "198.51.100.10")
	base, cookie, orgID := nodesOrg(t, e.q, e.orgSvc, "link-relay@k.local", "OrgLinkRelay")

	if cfg := e.gatewayConfig(t); len(cfg) != 0 {
		t.Fatalf("link off, panel off: want {}, got %v", cfg)
	}
	if rec := postForm(t, e.h, base+"/link/role", cookie, url.Values{"role": {"relay"}, "link_host": {"Link.Example.com."}}); hasErrFlash(rec) {
		t.Fatalf("set relay: %q", flashCookieValue(rec))
	}
	if e.link.Role() != link.RoleRelay {
		t.Fatalf("role %q, want relay", e.link.Role())
	}
	cfg := e.gatewayConfig(t)
	if _, ok := cfg["http"]; ok {
		t.Fatal("panel off: no http section expected")
	}
	tcp, _ := cfg["tcp"].(map[string]any)
	routers, _ := tcp["routers"].(map[string]any)
	r, _ := routers["krill-link"].(map[string]any)
	if r["rule"] != "HostSNI(`link.example.com`)" {
		t.Fatalf("tcp router %v", r)
	}
	svc := tcp["services"].(map[string]any)["krill-link"].(map[string]any)["loadBalancer"].(map[string]any)
	servers := svc["servers"].([]any)
	if addr := servers[0].(map[string]any)["address"].(string); !strings.HasPrefix(addr, "198.51.100.10:") {
		t.Fatalf("upstream %q, want the advertise address and the link port", addr)
	}

	// With the panel domain on, both are served.
	if rec := postForm(t, e.h, base+"/panel-domain", cookie, url.Values{"host": {"krill.example.com"}}); hasErrFlash(rec) {
		t.Fatalf("set panel: %q", flashCookieValue(rec))
	}
	cfg = e.gatewayConfig(t)
	if _, ok := cfg["http"]; !ok {
		t.Fatal("panel on: its http section must stay")
	}
	if _, ok := cfg["tcp"]; !ok {
		t.Fatal("panel on: the link route must stay")
	}

	// The panel cannot take the link host, nor an app.
	if rec := postForm(t, e.h, base+"/panel-domain", cookie, url.Values{"host": {linkHost}}); !flashHas(rec, hostshare.KeyReservedLink) {
		t.Fatalf("panel on the link host: %q", flashCookieValue(rec))
	}
	p, _ := e.orgSvc.CreateProject(context.Background(), orgID, "Proj", "")
	env, _ := e.orgSvc.CreateEnvironment(context.Background(), p.ID, "production")
	appsURL := base + "/projects/" + i64(p.ID) + "/environments/" + i64(env.ID) + "/apps"
	rec := postForm(t, e.h, appsURL, cookie, url.Values{
		"name": {"web"}, "source_type": {"image"}, "image": {"nginx:alpine"}, "port": {"80"}, "domain": {linkHost},
	})
	if !flashHas(rec, hostshare.KeyReservedLink) {
		t.Fatalf("an app on the link host: %q", flashCookieValue(rec))
	}

	// A release without the link let a domain take the host: the route is
	// not published, and the page shows both records.
	rec = postForm(t, e.h, appsURL, cookie, url.Values{
		"name": {"web"}, "source_type": {"image"}, "image": {"nginx:alpine"}, "port": {"80"}, "domain": {"web.example.com"},
	})
	if hasErrFlash(rec) {
		t.Fatalf("create app: %q", flashCookieValue(rec))
	}
	apps, err := e.q.ListApplicationsByEnvironment(context.Background(), env.ID)
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps %v %v", apps, err)
	}
	if _, err := e.q.CreateDomain(context.Background(), db.CreateDomainParams{ApplicationID: apps[0].ID, Host: linkHost}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.gatewayConfig(t)["tcp"]; ok {
		t.Fatal("a link host held by a domain must not be routed")
	}
	page := getPage(t, e.h, base+"/link", cookie, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Proj/production/web") {
		t.Fatalf("link page must show the conflicting domain, got %d", page.Code)
	}
}

// An ingress host kept by a rollback from stage 2 on the link host also keeps
// the link's route unpublished.
func TestLinkRouteNotPublishedOverAnIngressHost(t *testing.T) {
	e := newLinkServer(t, "198.51.100.10")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "link-ingress@k.local", "OrgLinkIngress")
	if rec := postForm(t, e.h, base+"/link/role", cookie, url.Values{"role": {"relay"}, "link_host": {linkHost}}); hasErrFlash(rec) {
		t.Fatalf("set relay: %q", flashCookieValue(rec))
	}
	if _, ok := e.gatewayConfig(t)["tcp"]; !ok {
		t.Fatal("a free link host must be routed")
	}
	peer, err := e.q.CreateLinkPeer(context.Background(), db.CreateLinkPeerParams{Name: "msk", CertSha256: "aa"})
	if err != nil {
		t.Fatal(err)
	}
	if err := insertIngressHost(context.Background(), e, peer.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.gatewayConfig(t)["tcp"]; ok {
		t.Fatal("a link host that is also an ingress host must not be routed")
	}
}

// The link listener must not share a port the lockdown keeps public: it
// would be reachable from the world, not from the gateway only.
func TestLinkRefusesAPublicPort(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", ListenAddr: ":8080", AdvertiseAddr: "198.51.100.10", LinkListenAddr: ":443"}
	orgSvc := org.NewService(q)
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	hosts := hostshare.NewStore(pool)
	srv.SetHostShare(hosts)
	l := link.NewService(pool, hosts, link.Config{AdvertiseAddr: cfg.AdvertiseAddr, ListenAddr: cfg.ListenAddr, LinkListenAddr: cfg.LinkListenAddr})
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	srv.SetLink(l)
	h := srv.Router()
	base, cookie, _ := nodesOrg(t, q, orgSvc, "link-port@k.local", "OrgLinkPort")
	for _, role := range []string{"client", "relay"} {
		rec := postForm(t, h, base+"/link/role", cookie, url.Values{"role": {role}, "link_host": {linkHost}})
		v, _ := url.QueryUnescape(flashCookieValue(rec))
		if !strings.Contains(v, i18n.T(i18n.WithLocale(context.Background(), "en"), "link.req.port_taken")) {
			t.Fatalf("%s on :443: %q", role, v)
		}
	}
	if l.Role() != link.RoleOff {
		t.Fatalf("role %q", l.Role())
	}
}

// A pairing code is shown once and only to the operator who made it; a code
// for an existing peer name is refused; the page never echoes it again.
func TestLinkPairingCodeShownOnce(t *testing.T) {
	e := newLinkServer(t, "198.51.100.10")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "link-code@k.local", "OrgLinkCode")
	if rec := postForm(t, e.h, base+"/link/role", cookie, url.Values{"role": {"relay"}, "link_host": {linkHost}}); hasErrFlash(rec) {
		t.Fatalf("set relay: %q", flashCookieValue(rec))
	}
	rec := postForm(t, e.h, base+"/link/codes", cookie, url.Values{"peer": {"msk"}})
	var code string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "krill_flash_link_code" {
			code = c.Value
			if !c.HttpOnly {
				t.Fatal("the code cookie must be HttpOnly")
			}
		}
	}
	pc, err := link.DecodeCode(code)
	if err != nil {
		t.Fatalf("flash code %q: %v", code, err)
	}
	if pc.Addr != linkHost+":443" || pc.SNI != linkHost {
		t.Fatalf("code %+v", pc)
	}
	req := httptest.NewRequest(http.MethodGet, base+"/link", nil)
	req.AddCookie(cookie)
	req.AddCookie(&http.Cookie{Name: "krill_flash_link_code", Value: code})
	page := httptest.NewRecorder()
	e.h.ServeHTTP(page, req)
	if !strings.Contains(page.Body.String(), code) {
		t.Fatal("the page must show the fresh code")
	}
	again := getPage(t, e.h, base+"/link", cookie, nil)
	if strings.Contains(again.Body.String(), code) {
		t.Fatal("the code must not be shown a second time")
	}
	if rec := postForm(t, e.h, base+"/link/codes", cookie, url.Values{"peer": {"Bad Name"}}); !flashHas(rec, "link.err.invalid_peer") {
		t.Fatalf("bad peer name: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, e.h, base+"/link/codes", cookie, url.Values{"peer": {"spb"}, "addr": {"nope"}}); !flashHas(rec, "link.err.invalid_addr") {
		t.Fatalf("bad address: %q", flashCookieValue(rec))
	}
}

// The client side: pairing needs the client role, a bad code is refused
// before any network call, forget and turning off work.
func TestLinkClientPage(t *testing.T) {
	e := newLinkServer(t, "")
	base, cookie, _ := nodesOrg(t, e.q, e.orgSvc, "link-client@k.local", "OrgLinkClient")
	if rec := postForm(t, e.h, base+"/link/pair", cookie, url.Values{"code": {"krl1_x"}}); !flashHas(rec, "link.err.not_client") {
		t.Fatalf("pair while off: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, e.h, base+"/link/role", cookie, url.Values{"role": {"client"}}); hasErrFlash(rec) {
		t.Fatalf("set client: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, e.h, base+"/link/pair", cookie, url.Values{"code": {"not a code"}}); !flashHas(rec, "link.err.bad_code") {
		t.Fatalf("bad code: %q", flashCookieValue(rec))
	}
	page := getPage(t, e.h, base+"/link", cookie, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="`+base+`/link/pair"`) {
		t.Fatalf("client page: %d", page.Code)
	}
	if rec := postForm(t, e.h, base+"/link/forget", cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("forget: %q", flashCookieValue(rec))
	}
	if rec := postForm(t, e.h, base+"/link/role", cookie, url.Values{"role": {"off"}}); hasErrFlash(rec) {
		t.Fatalf("off: %q", flashCookieValue(rec))
	}
	if e.link.Role() != link.RoleOff {
		t.Fatalf("role %q after off", e.link.Role())
	}
}

// While the link role is on, the lockdown admits the link port from the
// gateway only, and that gateway-only rule must not read as "the UI port is
// closed" on the panel domain page.
func TestLinkPortUnderLockdown(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	sec, err := panel.EnsureSecret(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	tokens := panel.DeriveTokens(sec)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", ListenAddr: ":8080", AdvertiseAddr: "localhost", LinkListenAddr: freeLinkPort(t)}
	orgSvc := org.NewService(q)
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	hosts := hostshare.NewStore(pool)
	srv.SetHostShare(hosts)
	fw := &hostFirewall{}
	srv.SetControlPlaneFirewall(fw)
	srv.SetPanelCertCheck(func(context.Context, string) error { return nil })
	upstream, uerr := panel.Upstream(cfg.AdvertiseAddr, cfg.ListenAddr)
	srv.SetPanelGateway(tokens, upstream, uerr)
	l := link.NewService(pool, hosts, link.Config{AdvertiseAddr: cfg.AdvertiseAddr, ListenAddr: cfg.ListenAddr, LinkListenAddr: cfg.LinkListenAddr})
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	srv.SetLink(l)
	h := srv.Router()
	e := &panelEnv{h: h, q: q, orgSvc: orgSvc, tokens: tokens, fw: fw}
	base, cookie, _ := nodesOrg(t, q, orgSvc, "link-fw@k.local", "OrgLinkFW")
	e.post(t, base+"/panel-domain", cookie, url.Values{"host": {panelHost}}, false)
	e.post(t, base+"/panel-domain/confirm", cookie, url.Values{}, true)
	postForm(t, h, base+"/firewall/lockdown", cookie, url.Values{})
	postForm(t, h, base+"/firewall/confirm", cookie, url.Values{})

	if rec := postForm(t, h, base+"/link/role", cookie, url.Values{"role": {"client"}}); hasErrFlash(rec) {
		t.Fatalf("set client: %q", flashCookieValue(rec))
	}
	// Turned on under an existing lockdown: the page asks to re-apply it.
	if body := getPage(t, h, base+"/link", cookie, nil).Body.String(); !strings.Contains(body, "/firewall") || !strings.Contains(body, "Re-apply the firewall") {
		t.Fatal("the link page must ask to re-apply the firewall")
	}
	postForm(t, h, base+"/firewall/lockdown", cookie, url.Values{})
	postForm(t, h, base+"/firewall/confirm", cookie, url.Values{})
	fw.mu.Lock()
	last := fw.rulesets[len(fw.rulesets)-1]
	fw.mu.Unlock()
	port := strings.TrimPrefix(cfg.LinkListenAddr, ":")
	if !strings.Contains(last, `iifname "docker_gwbridge" tcp dport { `+port+` } accept`) {
		t.Fatalf("the link port must be gateway-only:\n%s", last)
	}
	if !strings.Contains(last, "tcp dport { 80, 443, 8080 } accept") {
		t.Fatalf("the UI port must stay public:\n%s", last)
	}
	if body := getPage(t, h, base+"/link", cookie, nil).Body.String(); strings.Contains(body, "Re-apply the firewall") {
		t.Fatal("after re-applying, the link page must not ask again")
	}
	req := httptest.NewRequest(http.MethodGet, base+"/panel-domain", nil)
	req.AddCookie(cookie)
	e.viaDomain(req, "203.0.113.7")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "never recorded") {
		t.Fatal("the link's gateway-only port must not read as a closed UI port")
	}
}

func insertIngressHost(ctx context.Context, e *linkEnv, peerID int64) error {
	_, err := e.pool.Exec(ctx, "INSERT INTO link_ingress_hosts (host, peer_id) VALUES ($1, $2)", linkHost, peerID)
	return err
}
