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

// TraefikSpec builds the spec for the Traefik Swarm service.
// Configured via CLI arguments; only the docker socket is mounted (no traefik.yml).
func TraefikSpec(network string) docker.ServiceSpec {
	return docker.ServiceSpec{
		Name:     "krill-traefik",
		Image:    "traefik:" + TraefikVersion,
		Replicas: 1,
		Network:  network,
		Args: []string{
			"--providers.swarm.endpoint=unix:///var/run/docker.sock",
			"--providers.swarm.exposedByDefault=false",
			"--providers.swarm.network=" + network,
			"--entrypoints.web.address=:80",
		},
		Ports: []docker.PortSpec{
			{Target: 80, Published: 80, Mode: "host"},
		},
		Mounts: []docker.MountSpec{
			{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true},
		},
		Constraints: []string{"node.role==manager"},
	}
}

// Bootstrap ensures the overlay network and the Traefik service (idempotent).
func Bootstrap(ctx context.Context, eng docker.Engine, network string) error {
	if err := eng.NetworkEnsure(ctx, network); err != nil {
		return err
	}
	return eng.ServiceDeploy(ctx, TraefikSpec(network))
}
