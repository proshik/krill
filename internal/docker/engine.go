package docker

import (
	"context"
	"io"
	"strconv"
)

// PortSpec — a published service port.
type PortSpec struct {
	Target    uint32
	Published uint32
	Mode      string // "host" | "ingress"
	UDP       bool
}

// MountSpec — a mount (host bind path or named volume).
type MountSpec struct {
	Type     string // "bind" | "volume"; empty => "bind"
	Source   string
	Target   string
	ReadOnly bool
}

// ServiceSpec — our neutral description of a Swarm service.
type ServiceSpec struct {
	Name        string
	Image       string // image:tag
	Command     []string
	Args        []string
	Env         map[string]string
	Labels      map[string]string // service-level (read by the Traefik swarm provider)
	Replicas    uint64
	Network     string
	Ports       []PortSpec
	Mounts      []MountSpec
	Constraints []string // e.g. node.role==manager
	DNSRR       bool     // true => EndpointSpec.Mode=dnsrr (for databases), otherwise vip
}

// ServiceState — the current state of a service in Swarm.
type ServiceState struct {
	Found   bool
	Running int
	Desired int
}

// Engine — a narrow, mockable interface to Docker/Swarm.
type Engine interface {
	NetworkEnsure(ctx context.Context, name string) error
	ServiceDeploy(ctx context.Context, spec ServiceSpec) error // create-or-rolling-update by name
	ServiceRemove(ctx context.Context, name string) error
	ServiceState(ctx context.Context, name string) (ServiceState, error)
	ServiceLogs(ctx context.Context, name string, follow bool) (io.ReadCloser, error)
	ServiceScale(ctx context.Context, name string, replicas uint64) error
	ImagePull(ctx context.Context, ref string, out io.Writer) error
	VolumeRemove(ctx context.Context, name string) error
}

// ServiceName builds the Swarm service name for an application from its id.
func ServiceName(appID int64) string {
	return "krill-" + strconv.FormatInt(appID, 10)
}

// BuildImageTag — the name of the locally built application image for a specific deployment.
func BuildImageTag(appID, deployID int64) string {
	return "krill-" + strconv.FormatInt(appID, 10) + ":" + strconv.FormatInt(deployID, 10)
}
