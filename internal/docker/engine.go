package docker

import (
	"context"
	"io"
	"strconv"
	"time"
)

// ExecSession is one interactive TTY exec attached to a container. Read yields
// the container's combined stdout+stderr (raw — not stdcopy-muxed under TTY);
// Write sends stdin; Resize updates the PTY size; Close ends the session.
type ExecSession interface {
	io.ReadWriteCloser
	Resize(ctx context.Context, rows, cols uint) error
}

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
	Owner    string // "uid:gid" to chown the volume to before deploy; "" => skip
}

// ServiceSpec — our neutral description of a Swarm service.
type ServiceSpec struct {
	Name         string
	Image        string // image:tag
	Command      []string
	Args         []string
	Env          map[string]string
	Labels       map[string]string // service-level (read by the Traefik swarm provider)
	Replicas     uint64
	Network      string
	Ports        []PortSpec
	Mounts       []MountSpec
	Constraints  []string // e.g. node.role==manager
	Global       bool     // true => Mode.Global (one task per matching node); Replicas ignored
	SpreadNodeID bool     // add a spread-over-node.id placement preference (distribute replicas)
	DNSRR        bool     // true => EndpointSpec.Mode=dnsrr (for databases), otherwise vip
	RegistryAuth string   // base64url(JSON) auth blob; goes into ServiceCreate/UpdateOptions, not the swarm spec

	MemoryLimitBytes   int64  // 0 = no limit
	NanoCPUs           int64  // 0 = no limit
	RestartCondition   string // "any" | "on-failure" | "none"; "" => "any"
	RestartMaxAttempts uint64 // 0 = unlimited
	Healthcheck        *HealthcheckSpec
}

// SwarmNode is one cluster node (from the Swarm API).
type SwarmNode struct {
	ID, Hostname, Role, Availability, State, Addr string
	Leader                                        bool
}

// TaskPlacement is one task of a service and the node it runs on.
type TaskPlacement struct{ NodeID, NodeName, State, Desired string }

// TaskInfo is one Swarm task tagged with its owning service and node. Unlike
// TaskPlacement (which is per-service and carries no service identity), Tasks()
// returns TaskInfo across ALL services in one sweep, so the topology view can
// place every app/DB on the cluster without an API call per service.
type TaskInfo struct {
	ServiceName string // Swarm service name (krill-<appID> or db_instances.app_name)
	NodeID      string
	NodeName    string // swarm hostname; "" if the task is not yet scheduled
	State       string
	Desired     string
}

// HealthcheckSpec configures the container healthcheck.
type HealthcheckSpec struct {
	Test        []string // e.g. ["CMD-SHELL", "curl -f http://localhost/ || exit 1"]
	Interval    time.Duration
	Timeout     time.Duration
	StartPeriod time.Duration
	Retries     int
}

// ServiceState — the current state of a service in Swarm.
type ServiceState struct {
	Found   bool
	Running int
	Desired int
	Failed  int // tasks (desired=running) currently failed/rejected — crash-loop signal
}

// ServiceProgress is rolling-update progress relative to a baseline task set
// captured before the deploy. Running/Failed count ONLY tasks whose ID is not
// in the excluded baseline — during a StartFirst rolling update the OLD task
// keeps running (same ID) until the new one is ready, so counting it (as
// ServiceState does) would declare every redeploy converged instantly.
// UpdateState surfaces swarm's rolling-update state ("", "updating",
// "completed", "rollback_started", "rollback_completed", "paused") so the
// deployer can detect a silent rollback-on-failure.
type ServiceProgress struct {
	Found       bool
	Desired     int
	Running     int      // non-excluded tasks currently running
	Failed      int      // non-excluded tasks failed/rejected
	UpdateState string   // swarm UpdateStatus.State; "" when never updated
	TaskIDs     []string // IDs of the desired-state=running tasks seen (the baseline for the next deploy)
}

// ContainerStat is a one-shot CPU/memory sample for a running container.
type ContainerStat struct {
	Component     string  // swarm service name (label) or container name
	CPUPct        float64 // % of total host CPU capacity (0..NCPU*100)
	MemBytes      int64
	MemLimitBytes int64 // raw cgroup limit (== host total when unconstrained)
	SelfControl   bool  // true for Krill's own container
}

// NodeInfo is host-level capacity.
type NodeInfo struct {
	MemTotal int64
	NCPU     int
}

// Engine — a narrow, mockable interface to Docker/Swarm.
type Engine interface {
	NetworkEnsure(ctx context.Context, name string) error
	ServiceDeploy(ctx context.Context, spec ServiceSpec) error // create-or-rolling-update by name
	ServiceRemove(ctx context.Context, name string) error
	ServiceState(ctx context.Context, name string) (ServiceState, error)
	ServiceStates(ctx context.Context, names []string) (map[string]ServiceState, error)          // bulk: one API round-trip for many services
	ServiceProgress(ctx context.Context, name string, exclude []string) (ServiceProgress, error) // deploy convergence relative to a pre-deploy baseline task set
	ServiceLogs(ctx context.Context, name string, follow bool) (io.ReadCloser, error)
	ServiceScale(ctx context.Context, name string, replicas uint64) error
	ServiceRestart(ctx context.Context, name string) error // force-restart current tasks without rebuilding
	ImagePull(ctx context.Context, ref string, out io.Writer) error
	VolumeRemove(ctx context.Context, name string) error
	VolumeArchive(ctx context.Context, volumeName string, out io.Writer, swarmNodeID string) error
	VolumeRestore(ctx context.Context, volumeName string, in io.Reader, swarmNodeID string) error
	VolumeRemoveOn(ctx context.Context, name, swarmNodeID string) error         // node-aware force remove
	VolumeExistsOn(ctx context.Context, name, swarmNodeID string) (bool, error) // guards implicit volume creation
	VolumeChown(ctx context.Context, volumeName string, uid, gid int, swarmNodeID string) error
	ServiceUpdateLabels(ctx context.Context, name string, labels map[string]string) error
	Exec(ctx context.Context, serviceName string, cmd []string, env []string, stdin io.Reader, stdout io.Writer) error
	ExecInteractive(ctx context.Context, serviceName string, cmd []string) (ExecSession, error)
	RegistryCheck(ctx context.Context, serverAddr, username, password string) error
	ListContainerStats(ctx context.Context) ([]ContainerStat, error)
	NodeInfo(ctx context.Context) (NodeInfo, error)
	Nodes(ctx context.Context) ([]SwarmNode, error)
	NodeSetAvailability(ctx context.Context, nodeID, availability string) error
	NodeRemove(ctx context.Context, nodeID string, force bool) error
	SwarmWorkerToken(ctx context.Context) (string, error)
	ServiceTasks(ctx context.Context, name string) ([]TaskPlacement, error)
	Tasks(ctx context.Context) ([]TaskInfo, error) // every running task across all services, for the topology view
	NodeSetLabel(ctx context.Context, nodeID, key, value string) error
	NodeDeleteLabel(ctx context.Context, nodeID, key string) error
	// ResolveDigest returns a digest-pinned reference (repo@sha256:…) for ref by
	// querying the registry (no pull). encodedAuth is the same X-Registry-Auth
	// blob used for ServiceCreate/Update; "" for public images.
	ResolveDigest(ctx context.Context, ref, encodedAuth string) (string, error)
}

// RemoteExecConfigurable is the optional capability to route exec to the node a
// container runs on. Only the real *dockerEngine implements it; main.go
// type-asserts, so the Engine interface and its test fakes stay unchanged.
type RemoteExecConfigurable interface {
	SetRemoteClientProvider(p RemoteClientProvider)
}

// ServiceName builds the Swarm service name for an application from its id.
func ServiceName(appID int64) string {
	return "krill-" + strconv.FormatInt(appID, 10)
}

// BuildImageTag — the name of the locally built application image for a specific deployment.
func BuildImageTag(appID, deployID int64) string {
	return "krill-" + strconv.FormatInt(appID, 10) + ":" + strconv.FormatInt(deployID, 10)
}
