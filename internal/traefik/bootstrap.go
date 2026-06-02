package traefik

import (
	"context"

	"github.com/proshik/krill/internal/docker"
)

// TraefikVersion is the Traefik image version.
// v3.6.1+ is required: in it the swarm provider auto-negotiates the Docker API
// (traefik#12253/#12256). Versions ≤3.6.0 hardcode API 1.24 and Engine 29.x
// rejects them ("client version 1.24 is too old"), so routers are not built.
const TraefikVersion = "v3.6.1"

// AcmeConfig holds the Let's Encrypt settings for the Traefik resolver.
type AcmeConfig struct {
	Email   string
	Staging bool
}

// TraefikSpec builds the spec for the Traefik Swarm service.
// Configured via CLI arguments; only the docker socket is mounted (no traefik.yml).
func TraefikSpec(network string, acme AcmeConfig) docker.ServiceSpec {
	args := []string{
		"--providers.swarm.endpoint=unix:///var/run/docker.sock",
		"--providers.swarm.exposedByDefault=false",
		"--providers.swarm.network=" + network,
		"--entrypoints.web.address=:80",
		"--entrypoints.websecure.address=:443",
		"--certificatesresolvers.le.acme.email=" + acme.Email,
		"--certificatesresolvers.le.acme.storage=/letsencrypt/acme.json",
		"--certificatesresolvers.le.acme.httpchallenge=true",
		"--certificatesresolvers.le.acme.httpchallenge.entrypoint=web",
	}
	if acme.Staging {
		args = append(args, "--certificatesresolvers.le.acme.caserver=https://acme-staging-v02.api.letsencrypt.org/directory")
	}
	return docker.ServiceSpec{
		Name:     "krill-traefik",
		Image:    "traefik:" + TraefikVersion,
		Replicas: 1,
		Network:  network,
		Args:     args,
		Ports: []docker.PortSpec{
			{Target: 80, Published: 80, Mode: "host"},
			{Target: 443, Published: 443, Mode: "host"},
		},
		Mounts: []docker.MountSpec{
			{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true},
			{Type: "volume", Source: "krill-traefik-acme", Target: "/letsencrypt"},
		},
		Constraints: []string{"node.role==manager"},
	}
}

// Bootstrap ensures the overlay network and the Traefik service (idempotent).
func Bootstrap(ctx context.Context, eng docker.Engine, network string, acme AcmeConfig) error {
	if err := eng.NetworkEnsure(ctx, network); err != nil {
		return err
	}
	return eng.ServiceDeploy(ctx, TraefikSpec(network, acme))
}
