package traefik

import (
	"context"

	"github.com/proshik/krill/internal/docker"
)

// TraefikVersion — версия образа Traefik.
const TraefikVersion = "v3.5.0"

// TraefikSpec строит спецификацию Swarm-сервиса Traefik.
// Конфигурируется CLI-аргументами; монтируется только docker-сокет (без traefik.yml).
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

// Bootstrap гарантирует overlay-сеть и сервис Traefik (идемпотентно).
func Bootstrap(ctx context.Context, eng docker.Engine, network string) error {
	if err := eng.NetworkEnsure(ctx, network); err != nil {
		return err
	}
	return eng.ServiceDeploy(ctx, TraefikSpec(network))
}
