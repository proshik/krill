package docker

import (
	"context"
	"io"
	"os"
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

// FileRef mounts a Swarm config or secret into the service's containers as a
// file. Swarm references an object by ID, which ServiceDeploy looks up from
// Name, so callers leave ID empty.
type FileRef struct {
	Name   string
	Target string      // configs: absolute path; secrets: file name under /run/secrets
	Mode   os.FileMode // 0 => 0444
	ID     string      `json:"-"`
}

// ServiceSpec — our neutral description of a Swarm service.
type ServiceSpec struct {
	Name     string
	Image    string // image:tag
	Command  []string
	Args     []string
	Env      map[string]string
	Labels   map[string]string // service-level (read by the Traefik swarm provider)
	Replicas uint64
	Network  string
	// Networks lists every overlay network the service attaches to; when it is
	// non-empty it supersedes Network. Only a service that has to live in more
	// than one network sets it — today that is just the Traefik gateway, which
	// must reach every organization's network while those stay isolated from
	// each other.
	Networks []string
	Ports    []PortSpec
	Mounts   []MountSpec
	Configs  []FileRef // Swarm configs mounted as files
	Secrets  []FileRef // Swarm secrets mounted under /run/secrets
	// ContainerLabels land on the containers, unlike Labels (service level):
	// a log collector reading the docker socket sees only these.
	ContainerLabels map[string]string
	Hostname        string // container hostname; Swarm templates allowed, e.g. {{.Node.Hostname}}
	// UpdateStopFirst stops the old task before starting its replacement — for
	// a task that holds something node-local, like an agent's WAL directory.
	UpdateStopFirst bool
	Constraints     []string // e.g. node.role==manager
	Global          bool     // true => Mode.Global (one task per matching node); Replicas ignored
	SpreadNodeID    bool     // add a spread-over-node.id placement preference (distribute replicas)
	DNSRR           bool     // true => EndpointSpec.Mode=dnsrr (for databases), otherwise vip
	RegistryAuth    string   // base64url(JSON) auth blob; goes into ServiceCreate/UpdateOptions, not the swarm spec

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

// TaskPlacement is one task of a service and the node it runs on. ID is the
// task's own Swarm ID — callers that captured a pre-deploy baseline of task
// IDs use it to tell a node still running its OLD task (before a rolling
// update reached it) from one already running the new spec, which State
// alone cannot: a stalled node's stale task still reports State "running".
type TaskPlacement struct{ ID, NodeID, NodeName, State, Desired string }

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

// UpdateSettled reports whether a service's last rolling update has come to
// rest. "" means the service was never updated.
func UpdateSettled(state string) bool {
	switch state {
	case "updating", "paused", "rollback_started", "rollback_paused":
		return false
	}
	return true
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
	// NetworkEnsure creates the overlay network if it is missing and reports
	// whether it actually created it. A recreated network gets a NEW id, and a
	// service's stored spec still refers to the old one — so a caller that
	// skips redundant deploys has to treat "created" as a reason to deploy.
	NetworkEnsure(ctx context.Context, name string) (created bool, err error)
	NetworkRemove(ctx context.Context, name string) error      // idempotent: "network not found" is not an error
	ServiceDeploy(ctx context.Context, spec ServiceSpec) error // create-or-rolling-update by name
	ServiceRemove(ctx context.Context, name string) error
	ServiceState(ctx context.Context, name string) (ServiceState, error)
	ServiceStates(ctx context.Context, names []string) (map[string]ServiceState, error)          // bulk: one API round-trip for many services
	ServiceProgress(ctx context.Context, name string, exclude []string) (ServiceProgress, error) // deploy convergence relative to a pre-deploy baseline task set
	ServiceLogs(ctx context.Context, name string, follow bool, tail int) (io.ReadCloser, error)  // tail bounds how many trailing lines the transport returns; callers must pass an explicit value, there is no default
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
	// ServiceLabels returns the service-level labels of a running service and
	// whether that service exists. It lets a caller tell an unchanged service
	// from one that needs redeploying: ServiceDeploy always bumps ForceUpdate,
	// so it recreates the task even when the spec is identical.
	ServiceLabels(ctx context.Context, name string) (map[string]string, bool, error)
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

// SwarmObjects is the optional capability to manage Swarm configs and secrets.
// Only the real *dockerEngine implements it; callers type-assert, so the Engine
// interface and its test fakes stay unchanged.
type SwarmObjects interface {
	// ConfigEnsure creates the config unless one with that name exists.
	ConfigEnsure(ctx context.Context, name string, data []byte, labels map[string]string) error
	// SecretEnsure creates the secret unless one with that name exists.
	SecretEnsure(ctx context.Context, name string, data []byte, labels map[string]string) error
	// PruneObjects removes the configs and secrets labelled key=value whose
	// names are not in keep, skipping any a service still uses.
	PruneObjects(ctx context.Context, labelKey, labelValue string, keep []string) error
}

var _ SwarmObjects = (*dockerEngine)(nil)

// ServiceName builds the Swarm service name for an application from its id.
func ServiceName(appID int64) string {
	return "krill-" + strconv.FormatInt(appID, 10)
}

// BuildImageTag — the name of the locally built application image for a specific deployment.
func BuildImageTag(appID, deployID int64) string {
	return "krill-" + strconv.FormatInt(appID, 10) + ":" + strconv.FormatInt(deployID, 10)
}

// TaskAddress is one IP a running task holds on one overlay network.
type TaskAddress struct {
	ServiceName  string
	NodeID       string
	NodeHostname string // "" when the node could not be resolved
	Network      string
	IP           string // without the prefix length
}

// TaskAddresser is the optional capability to list the overlay addresses of
// every running task. Only *dockerEngine implements it.
type TaskAddresser interface {
	TaskAddresses(ctx context.Context) ([]TaskAddress, error)
}

// ServiceInspector is the optional capability to read the container labels a
// service's CURRENT spec starts tasks with (not the service-level labels
// ServiceLabels returns).
type ServiceInspector interface {
	ServiceContainerLabels(ctx context.Context, name string) (map[string]string, bool, error)
}

var (
	_ TaskAddresser    = (*dockerEngine)(nil)
	_ ServiceInspector = (*dockerEngine)(nil)
)
