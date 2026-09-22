package traefik

import (
	"strings"
	"testing"
)

func TestAppLabelsProtection(t *testing.T) {
	const svc = "krill-7"
	t.Run("ip-allowlist on non-TLS attaches to base router", func(t *testing.T) {
		l := AppLabels(svc, []Domain{{Host: "a.example.com", Exposed: true, AllowedIPs: []string{"10.0.0.0/8", "1.2.3.4/32"}}}, 80, "krill-net", nil)
		base := svc + "-d0"
		if got := l["traefik.http.middlewares."+base+"-ipallow.ipallowlist.sourcerange"]; got != "10.0.0.0/8,1.2.3.4/32" {
			t.Fatalf("sourcerange = %q", got)
		}
		if got := l["traefik.http.routers."+base+".middlewares"]; got != base+"-ipallow" {
			t.Fatalf("middlewares = %q", got)
		}
	})
	t.Run("basic-auth on TLS attaches to secure router, not redirect router", func(t *testing.T) {
		l := AppLabels(svc, []Domain{{Host: "a.example.com", Exposed: true, TLS: true, BasicAuthUsers: []string{"admin:$2a$10$abc"}}}, 80, "krill-net", nil)
		base := svc + "-d0"
		if got := l["traefik.http.middlewares."+base+"-auth.basicauth.users"]; got != "admin:$2a$10$abc" {
			t.Fatalf("users = %q", got)
		}
		if got := l["traefik.http.routers."+base+"s.middlewares"]; got != base+"-auth" {
			t.Fatalf("secure middlewares = %q", got)
		}
		if got := l["traefik.http.routers."+base+".middlewares"]; got != base+"-redirect" {
			t.Fatalf("redirect router middlewares = %q", got)
		}
	})
	t.Run("both chain ipallow then auth", func(t *testing.T) {
		l := AppLabels(svc, []Domain{{Host: "a.example.com", Exposed: true, AllowedIPs: []string{"1.2.3.4/32"}, BasicAuthUsers: []string{"u:$2a$10$x"}}}, 80, "krill-net", nil)
		base := svc + "-d0"
		if got := l["traefik.http.routers."+base+".middlewares"]; got != base+"-ipallow,"+base+"-auth" {
			t.Fatalf("chain = %q", got)
		}
	})
	t.Run("empty adds no middleware", func(t *testing.T) {
		l := AppLabels(svc, []Domain{{Host: "a.example.com", Exposed: true}}, 80, "krill-net", nil)
		if _, ok := l["traefik.http.routers."+svc+"-d0.middlewares"]; ok {
			t.Fatal("unexpected middlewares on plain exposed domain")
		}
	})
}

func TestAppLabelsPlain(t *testing.T) {
	l := AppLabels("krill-1", []Domain{{Host: "a.example.com", TLS: false, Exposed: true}}, 80, "krill-net", nil)
	if l["traefik.enable"] != "true" {
		t.Fatal("not enabled")
	}
	if l["traefik.http.routers.krill-1-d0.rule"] != "Host(`a.example.com`)" {
		t.Errorf("rule = %q", l["traefik.http.routers.krill-1-d0.rule"])
	}
	if l["traefik.http.routers.krill-1-d0.entrypoints"] != "web" {
		t.Error("plain domain must be on web")
	}
	if _, ok := l["traefik.http.routers.krill-1-d0.tls.certresolver"]; ok {
		t.Error("plain domain must not have a certresolver")
	}
	if l["traefik.http.services.krill-1.loadbalancer.server.port"] != "80" {
		t.Error("service port wrong")
	}
}

func TestAppLabelsTLS(t *testing.T) {
	l := AppLabels("krill-1", []Domain{{Host: "b.example.com", TLS: true, Exposed: true}}, 3000, "krill-net", nil)
	if l["traefik.http.routers.krill-1-d0s.entrypoints"] != "websecure" {
		t.Error("tls domain secure router must be on websecure")
	}
	if l["traefik.http.routers.krill-1-d0s.tls.certresolver"] != "le" {
		t.Error("missing certresolver le")
	}
	if l["traefik.http.routers.krill-1-d0s.rule"] != "Host(`b.example.com`)" {
		t.Error("secure rule wrong")
	}
	if l["traefik.http.routers.krill-1-d0.entrypoints"] != "web" {
		t.Error("redirect router must be on web")
	}
	mw := l["traefik.http.routers.krill-1-d0.middlewares"]
	if mw == "" {
		t.Error("redirect router must have a middleware")
	}
	if l["traefik.http.middlewares."+mw+".redirectscheme.scheme"] != "https" {
		t.Errorf("redirect middleware not configured: mw=%q", mw)
	}
}

func TestDomainRule(t *testing.T) {
	if got := domainRule("a.example.com", nil, nil); got != "Host(`a.example.com`)" {
		t.Errorf("no-paths rule = %q", got)
	}
	got := domainRule("a.example.com", []string{"/webhook", "/healthz"}, nil)
	want := "Host(`a.example.com`) && (PathPrefix(`/webhook`) || PathPrefix(`/healthz`))"
	if got != want {
		t.Errorf("paths rule = %q want %q", got, want)
	}
}

func TestAppLabelsPaths(t *testing.T) {
	l := AppLabels("krill-1", []Domain{{Host: "a.example.com", Exposed: true, Paths: []string{"/webhook"}}}, 80, "krill-net", nil)
	if l["traefik.http.routers.krill-1-d0.rule"] != "Host(`a.example.com`) && (PathPrefix(`/webhook`))" {
		t.Errorf("unexpected rule: %q", l["traefik.http.routers.krill-1-d0.rule"])
	}
}

func TestAppLabelsSkipsNonExposed(t *testing.T) {
	l := AppLabels("krill-1", []Domain{{Host: "pub.example.com", Exposed: true}, {Host: "priv.example.com", Exposed: false}}, 80, "krill-net", nil)
	if l["traefik.http.routers.krill-1-d0.rule"] != "Host(`pub.example.com`)" {
		t.Errorf("exposed domain missing: %q", l["traefik.http.routers.krill-1-d0.rule"])
	}
	if _, ok := l["traefik.http.routers.krill-1-d1.rule"]; ok {
		t.Errorf("non-exposed domain should not emit a router")
	}
}

func TestAppLabelsNoneExposed(t *testing.T) {
	l := AppLabels("krill-1", []Domain{{Host: "a.example.com", Exposed: false}}, 80, "krill-net", nil)
	if l["traefik.enable"] != "false" || len(l) != 1 {
		t.Errorf("expected only traefik.enable=false, got %v", l)
	}
}

func TestDomainRuleHidesMetricsPaths(t *testing.T) {
	got := domainRule("a.example", nil, []string{"/metrics", "/q/metrics"})
	want := "Host(`a.example`) && !(PathRegexp(`(?i)^/metrics(?:[/;].*)?$`) || PathRegexp(`(?i)^/q/metrics(?:[/;].*)?$`))"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	got = domainRule("a.example", []string{"/api"}, []string{"/metrics"})
	want = "Host(`a.example`) && (PathPrefix(`/api`)) && !(PathRegexp(`(?i)^/metrics(?:[/;].*)?$`))"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if domainRule("a.example", nil, nil) != "Host(`a.example`)" {
		t.Fatal("no hidden paths → rule unchanged")
	}
}

func TestAppLabelsHidesOnEveryServingRouter(t *testing.T) {
	l := AppLabels("krill-7", []Domain{{Host: "a.example", TLS: true, Exposed: true}, {Host: "b.example", Exposed: true}}, 8080, "net", []string{"/metrics"})
	for _, k := range []string{"traefik.http.routers.krill-7-d0s.rule", "traefik.http.routers.krill-7-d0.rule", "traefik.http.routers.krill-7-d1.rule"} {
		if !strings.Contains(l[k], "!(PathRegexp(`(?i)^/metrics(?:") {
			t.Errorf("%s = %q: metrics path not excluded", k, l[k])
		}
	}
}
