package traefik

import (
	"strings"
	"testing"
)

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestTraefikSpecTLS(t *testing.T) {
	s := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c", Staging: false}, PanelProvider{})
	if !hasArg(s.Args, "--entrypoints.websecure.address=:443") {
		t.Error("missing websecure entrypoint")
	}
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.email=a@b.c") {
		t.Error("missing acme email")
	}
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.httpchallenge.entrypoint=web") {
		t.Error("missing http challenge entrypoint")
	}
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.storage=/letsencrypt/acme.json") {
		t.Error("missing acme storage")
	}
	var has443, hasVol bool
	for _, p := range s.Ports {
		if p.Target == 443 && p.Published == 443 {
			has443 = true
		}
	}
	for _, m := range s.Mounts {
		if m.Type == "volume" && m.Target == "/letsencrypt" {
			hasVol = true
		}
	}
	if !has443 || !hasVol {
		t.Errorf("443 port=%v acme volume=%v", has443, hasVol)
	}
	for _, a := range s.Args {
		if strings.Contains(a, "caserver") {
			t.Error("prod must not set a staging caserver")
		}
	}
}

func TestTraefikSpecStaging(t *testing.T) {
	s := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c", Staging: true}, PanelProvider{})
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory") {
		t.Error("staging caserver missing")
	}
}

func TestTraefikSpecPanelProvider(t *testing.T) {
	plain := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, PanelProvider{})
	for _, a := range plain.Args {
		if strings.HasPrefix(a, "--providers.http.") {
			t.Fatalf("no endpoint must leave the HTTP provider out, got %q", a)
		}
	}
	p := PanelProvider{Endpoint: "http://198.51.100.10:8080/_krill/gateway/config", Header: "X-Krill-Gateway-Provider", Token: "tok"}
	s := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, p)
	for _, want := range []string{
		"--providers.http.endpoint=http://198.51.100.10:8080/_krill/gateway/config",
		"--providers.http.pollInterval=5s",
		"--providers.http.headers.X-Krill-Gateway-Provider=tok",
		"--providers.swarm.endpoint=unix:///var/run/docker.sock",
	} {
		if !hasArg(s.Args, want) {
			t.Errorf("missing %q", want)
		}
	}
	if s.Labels[specHashLabel] == plain.Labels[specHashLabel] {
		t.Error("enabling the provider must change the fingerprint so the gateway is redeployed once")
	}
	if again := TraefikSpec([]string{"krill-net"}, AcmeConfig{Email: "a@b.c"}, p); again.Labels[specHashLabel] != s.Labels[specHashLabel] {
		t.Error("the same provider must fingerprint identically, or every start would redeploy the gateway")
	}
}
