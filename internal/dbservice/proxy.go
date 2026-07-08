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

// proxySpecTarget builds one socat proxy service: pinned to the manager,
// attached to the overlay so it resolves the DB service by name,
// host-publishing hostPort on the manager's public IP and forwarding to
// containerPort on appName.
func proxySpecTarget(name string, hostPort int32, appName string, containerPort uint32, network string) docker.ServiceSpec {
	port := uint32(hostPort)
	return docker.ServiceSpec{
		Name:        name,
		Image:       socatImage,
		Args:        []string{fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", port), fmt.Sprintf("TCP:%s:%d", appName, containerPort)},
		Replicas:    1,
		Network:     network,
		Constraints: []string{"node.role==manager"},
		Ports:       []docker.PortSpec{{Target: port, Published: port, Mode: "host"}},
	}
}

// reconcileProxy brings the control-plane proxy/proxies in line with the
// instance's driver-declared external targets: one socat service per target
// with a non-nil HostPort (deployed/updated), one ServiceRemove per target
// whose HostPort is nil (cleared). Today every driver (postgres, redis)
// returns exactly one target ("" suffix), so this is a one-proxy-per-instance
// no-op change; a future N-target driver (e.g. minio: data + console) gets
// one proxy per published port for free. Best-effort — a failure is logged by
// the caller; the DB itself is unaffected.
func (s *Service) reconcileProxy(ctx context.Context, inst Instance) error {
	if s.engine == nil {
		return nil
	}
	for _, t := range drivers.Registry.MustGet(inst.Engine).ExternalTargets(inst) {
		name := proxyName(inst.ID) + t.Suffix
		if t.HostPort == nil {
			if err := s.engine.ServiceRemove(ctx, name); err != nil && !isNotFound(err) {
				return fmt.Errorf("remove proxy %s: %w", name, err)
			}
			continue
		}
		if err := s.engine.ImagePull(ctx, socatImage, io.Discard); err != nil {
			return fmt.Errorf("pull socat: %w", err)
		}
		if err := s.engine.ServiceDeploy(ctx, proxySpecTarget(name, *t.HostPort, inst.AppName, t.ContainerPort, s.network)); err != nil {
			return fmt.Errorf("deploy proxy %s: %w", name, err)
		}
	}
	return nil
}
