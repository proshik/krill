package dbservice

import (
	"context"
	"fmt"
	"io"

	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
)

// socatImage is the pinned TCP-forward proxy used to expose a DB instance on the
// control-plane's public IP.
const socatImage = "alpine/socat@sha256:188fe0a22182f81c16def9d1137930121eaa3023f427164e32c0d2f118f79dd6"

// proxyName is the Swarm service name of the control-plane proxy for an instance.
func proxyName(instanceID int64) string { return fmt.Sprintf("krill-dbproxy-%d", instanceID) }

// proxySpec builds the socat proxy service: pinned to the manager, attached to
// the overlay so it resolves the DB service by name, host-publishing the
// instance's external_port on the manager's public IP.
func proxySpec(inst Instance, network string) docker.ServiceSpec {
	// Single-port today: the instance's driver may expose several external
	// targets (e.g. a future minio data+console pair); take the first (the
	// primary, "" suffix) — matches the current one-proxy-per-instance shape.
	target := drivers.Registry.MustGet(inst.Engine).ExternalTargets(inst)[0].ContainerPort
	port := uint32(*inst.ExternalPort)
	return docker.ServiceSpec{
		Name:        proxyName(inst.ID),
		Image:       socatImage,
		Args:        []string{fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", port), fmt.Sprintf("TCP:%s:%d", inst.AppName, target)},
		Replicas:    1,
		Network:     network,
		Constraints: []string{"node.role==manager"},
		Ports:       []docker.PortSpec{{Target: port, Published: port, Mode: "host"}},
	}
}

// reconcileProxy brings the control-plane proxy in line with the instance's
// external_port: deploy it when set, remove it when cleared. Best-effort — a
// failure is logged by the caller; the DB itself is unaffected.
func (s *Service) reconcileProxy(ctx context.Context, inst Instance) error {
	if s.engine == nil {
		return nil
	}
	if inst.ExternalPort == nil {
		if err := s.engine.ServiceRemove(ctx, proxyName(inst.ID)); err != nil && !isNotFound(err) {
			return fmt.Errorf("remove proxy: %w", err)
		}
		return nil
	}
	if err := s.engine.ImagePull(ctx, socatImage, io.Discard); err != nil {
		return fmt.Errorf("pull socat: %w", err)
	}
	return s.engine.ServiceDeploy(ctx, proxySpec(inst, s.network))
}
