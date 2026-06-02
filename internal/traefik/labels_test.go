package traefik

import "testing"

func TestAppLabelsPlain(t *testing.T) {
	l := AppLabels("krill-1", []Domain{{Host: "a.example.com", TLS: false}}, 80, "krill-net")
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
	l := AppLabels("krill-1", []Domain{{Host: "b.example.com", TLS: true}}, 3000, "krill-net")
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
