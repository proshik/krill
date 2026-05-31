package traefik

import (
	"strings"
	"testing"
)

func TestTraefikSpec(t *testing.T) {
	s := TraefikSpec("krill-net")
	if s.Name != "krill-traefik" {
		t.Errorf("name = %q", s.Name)
	}
	if !strings.HasPrefix(s.Image, "traefik:") {
		t.Errorf("image = %q", s.Image)
	}
	if len(s.Ports) == 0 || s.Ports[0].Mode != "host" {
		t.Error("port 80 must be host-mode")
	}
	var hasSock bool
	for _, m := range s.Mounts {
		if m.Source == "/var/run/docker.sock" {
			hasSock = true
		}
	}
	if !hasSock {
		t.Error("docker socket must be mounted")
	}
	joined := strings.Join(s.Args, " ")
	if !strings.Contains(joined, "--providers.swarm.network=krill-net") {
		t.Errorf("provider network arg missing: %v", s.Args)
	}
	if len(s.Constraints) == 0 {
		t.Error("expected manager constraint")
	}
	if s.Env["DOCKER_API_VERSION"] == "" {
		t.Error("DOCKER_API_VERSION must be set so Traefik's docker client negotiates a supported API")
	}
}
