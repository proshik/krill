package panel

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	ok := map[string]string{
		"krill.proshik.ru":   "krill.proshik.ru",
		" Krill.Example.COM": "krill.example.com",
		"panel.example.com.": "panel.example.com",
		"a-b.c1.example.io":  "a-b.c1.example.io",
	}
	for in, want := range ok {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "localhost", "195.2.75.130", "::1", "https://krill.example.com",
		"krill.example.com:443", "krill.example.com/path", "*.example.com",
		"-krill.example.com", "krill_.example.com", "krill.example.123",
		"a`) || Host(`b.example.com", "krill example.com",
	}
	for _, in := range bad {
		if got, err := NormalizeHost(in); !errors.Is(err, ErrInvalidHost) {
			t.Errorf("NormalizeHost(%q) = %q, %v; want ErrInvalidHost", in, got, err)
		}
	}
}

func TestUpstream(t *testing.T) {
	cases := []struct {
		adv, listen, want string
		err               error
	}{
		{"195.2.75.130", ":8080", "http://195.2.75.130:8080", nil},
		{"195.2.75.130", "0.0.0.0:9000", "http://195.2.75.130:9000", nil},
		{"2001:db8::1", "[::]:8080", "http://[2001:db8::1]:8080", nil},
		{"", ":8080", "", ErrAdvertiseUnset},
		{"  ", ":8080", "", ErrAdvertiseUnset},
		{"195.2.75.130", "127.0.0.1:8080", "", ErrListenLoopback},
		{"195.2.75.130", "localhost:8080", "", ErrListenLoopback},
		{"195.2.75.130", "[::1]:8080", "", ErrListenLoopback},
	}
	for _, c := range cases {
		got, err := Upstream(c.adv, c.listen)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("Upstream(%q,%q) err = %v; want %v", c.adv, c.listen, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Upstream(%q,%q) = %q, %v; want %q", c.adv, c.listen, got, err, c.want)
		}
	}
	for _, listen := range []string{"8080", ":http-alt", ":0", ":70000"} {
		if _, err := Upstream("195.2.75.130", listen); err == nil {
			t.Errorf("Upstream with listen %q: want an error", listen)
		}
	}
}

func TestDeriveTokens(t *testing.T) {
	a := DeriveTokens("secret-a")
	if a.Provider == "" || a.Forwarded == "" || a.Provider == a.Forwarded {
		t.Fatalf("tokens must be non-empty and distinct: %+v", a)
	}
	if DeriveTokens("secret-a") != a {
		t.Fatal("derivation must be deterministic, or the gateway spec would change on every start")
	}
	if b := DeriveTokens("secret-b"); b.Provider == a.Provider || b.Forwarded == a.Forwarded {
		t.Fatal("different secrets must give different tokens")
	}
}

func TestMatches(t *testing.T) {
	if Matches("", "") || Matches("x", "") || Matches("", "x") {
		t.Fatal("empty values must never match")
	}
	if !Matches("abc", "abc") || Matches("abd", "abc") {
		t.Fatal("Matches compares values")
	}
}

func TestDynamicConfigOff(t *testing.T) {
	for _, s := range []Settings{{}, {Host: "krill.example.com", State: StateOff}, {State: StatePending}} {
		if got := DynamicConfig(s, "http://1.2.3.4:8080", "tok"); len(got) != 0 {
			t.Errorf("DynamicConfig(%+v) = %v; want empty", s, got)
		}
	}
	if got := DynamicConfig(Settings{Host: "krill.example.com", State: StateActive}, "", "tok"); len(got) != 0 {
		t.Errorf("no upstream must give an empty config, got %v", got)
	}
}

// decode round-trips the config through JSON, which is what the gateway reads.
func decode(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, p)
		}
		cur, ok = mm[p]
		if !ok {
			t.Fatalf("path %v: missing %q", path, p)
		}
	}
	return cur
}

func TestDynamicConfigRoutes(t *testing.T) {
	for _, state := range []string{StatePending, StateActive} {
		cfg := decode(t, DynamicConfig(Settings{Host: "krill.example.com", State: state}, "http://195.2.75.130:8080", "fwd-token"))
		https := dig(t, cfg, "http", "routers", "krill-panel-https").(map[string]any)
		if https["rule"] != "Host(`krill.example.com`)" {
			t.Errorf("%s: https rule = %v", state, https["rule"])
		}
		if dig(t, https, "tls", "certResolver") != "le" {
			t.Errorf("%s: https router must use the le resolver", state)
		}
		if mws := https["middlewares"].([]any); len(mws) != 1 || mws[0] != "krill-panel-forwarded" {
			t.Errorf("%s: https middlewares = %v", state, mws)
		}
		if p := https["priority"].(float64); p < 100000 {
			t.Errorf("%s: panel routers must outrank app routers for the same host, priority %v", state, p)
		}
		http := dig(t, cfg, "http", "routers", "krill-panel-http").(map[string]any)
		if eps := http["entryPoints"].([]any); len(eps) != 1 || eps[0] != "web" {
			t.Errorf("%s: http router entrypoints = %v", state, eps)
		}
		if dig(t, cfg, "http", "middlewares", "krill-panel-redirect", "redirectScheme", "scheme") != "https" {
			t.Errorf("%s: plain HTTP must redirect to HTTPS", state)
		}
		if dig(t, cfg, "http", "middlewares", "krill-panel-forwarded", "headers", "customRequestHeaders", ForwardedHeader) != "fwd-token" {
			t.Errorf("%s: the gateway must stamp proxied requests with the forwarded token", state)
		}
		servers := dig(t, cfg, "http", "services", "krill-panel", "loadBalancer", "servers").([]any)
		if len(servers) != 1 || servers[0].(map[string]any)["url"] != "http://195.2.75.130:8080" {
			t.Errorf("%s: servers = %v", state, servers)
		}
		routers := dig(t, cfg, "http", "routers").(map[string]any)
		if _, ok := routers["krill-panel-machine"]; ok {
			t.Errorf("%s: no allowlist needs no separate machine router", state)
		}
	}
}

func TestDynamicConfigAllowlist(t *testing.T) {
	cfg := decode(t, DynamicConfig(Settings{
		Host: "krill.example.com", State: StateActive, AllowedIPs: []string{"203.0.113.4/32", "10.0.0.0/8"},
	}, "http://195.2.75.130:8080", "fwd-token"))
	rng := dig(t, cfg, "http", "middlewares", "krill-panel-ipallow", "ipAllowList", "sourceRange").([]any)
	if len(rng) != 2 || rng[0] != "203.0.113.4/32" {
		t.Fatalf("sourceRange = %v", rng)
	}
	https := dig(t, cfg, "http", "routers", "krill-panel-https").(map[string]any)
	if mws := https["middlewares"].([]any); len(mws) != 2 || mws[0] != "krill-panel-ipallow" {
		t.Fatalf("the UI router must filter by IP first: %v", mws)
	}
	machine := dig(t, cfg, "http", "routers", "krill-panel-machine").(map[string]any)
	rule := machine["rule"].(string)
	for _, p := range []string{"/webhooks/", "/api/", "/mcp"} {
		if !strings.Contains(rule, "PathPrefix(`"+p+"`)") {
			t.Errorf("machine router rule %q lacks %s", rule, p)
		}
	}
	if machine["priority"].(float64) <= https["priority"].(float64) {
		t.Error("the machine router must outrank the UI router, or the allowlist would catch webhooks")
	}
	for _, m := range machine["middlewares"].([]any) {
		if m == "krill-panel-ipallow" {
			t.Error("webhooks and the agent API authenticate themselves and must not be IP-filtered")
		}
	}
}

func TestViaGateway(t *testing.T) {
	r := httptest.NewRequest("GET", "http://krill.example.com/", nil)
	if ViaGateway(r, "tok") || SecureViaGateway(r, "tok") {
		t.Fatal("a request without the header is direct")
	}
	r.Header.Set(ForwardedHeader, "wrong")
	r.Header.Set("X-Forwarded-Proto", "https")
	if ViaGateway(r, "tok") || SecureViaGateway(r, "tok") {
		t.Fatal("a wrong token is direct, whatever it says about the scheme")
	}
	r.Header.Set(ForwardedHeader, "tok")
	if !SecureViaGateway(r, "tok") {
		t.Fatal("gateway + https must be secure")
	}
	r.Header.Set("X-Forwarded-Proto", "http")
	if !ViaGateway(r, "tok") || SecureViaGateway(r, "tok") {
		t.Fatal("gateway over plain http is not secure")
	}
	if ViaGateway(r, "") {
		t.Fatal("an unwired token must never match")
	}
}

func TestRequestHost(t *testing.T) {
	r := httptest.NewRequest("GET", "http://Krill.Example.com:443/", nil)
	if got := RequestHost(r); got != "krill.example.com" {
		t.Fatalf("RequestHost = %q", got)
	}
}
