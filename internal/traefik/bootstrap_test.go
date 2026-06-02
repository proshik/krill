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
	s := TraefikSpec("krill-net", AcmeConfig{Email: "a@b.c", Staging: false})
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
	s := TraefikSpec("krill-net", AcmeConfig{Email: "a@b.c", Staging: true})
	if !hasArg(s.Args, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory") {
		t.Error("staging caserver missing")
	}
}
