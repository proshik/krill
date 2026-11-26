// Package client talks to a Krill server's agent API over HTTP.
//
// The response types here deliberately duplicate the shapes in internal/api
// rather than importing them. Those are server types and will change for
// server reasons; a released krill-cli binary has to keep decoding whatever
// the server it was pointed at sends. Importing them would make every
// server-side struct edit a silent wire-compatibility decision nobody
// notices they are taking. client_drift_test.go compares the two by
// reflection so the divergence is reported by a test rather than by a user,
// and the dependency arrow only ever points CLI -> api, never back.
package client

// Whoami is the caller's resolved identity.
type Whoami struct {
	UserID   int64  `json:"user_id"`
	OrgID    int64  `json:"org_id"`
	OrgName  string `json:"org_name"`
	Level    string `json:"level"`
	Role     string `json:"role"`
	CanWrite bool   `json:"can_write"`
	// Instance is nil on a server older than the agent API diagnostics,
	// which sends no instance at all: a pointer keeps that absence in the
	// --json output instead of inventing an empty object.
	Instance *Instance `json:"instance,omitempty"`
}

// Instance is the Krill installation that answered.
type Instance struct {
	URL     string `json:"url,omitempty"`
	Version string `json:"version"`
}

// InstanceVersion is the answering installation's version, "" when the
// server does not report one.
func (w Whoami) InstanceVersion() string {
	if w.Instance == nil {
		return ""
	}
	return w.Instance.Version
}

// App is one application. Image is the repository alone; Tag is what it is
// configured to deploy, and is present for image apps only.
type App struct {
	ID         int64    `json:"id"`
	Path       string   `json:"path"`
	Name       string   `json:"name"`
	SourceType string   `json:"source_type"`
	Image      string   `json:"image,omitempty"`
	Tag        string   `json:"tag,omitempty"`
	Status     string   `json:"status"`
	Domains    []string `json:"domains,omitempty"`
}

// AppStatus is App plus live facts. The server flattens these into one
// object, so the fields sit alongside App's rather than nested.
type AppStatus struct {
	App
	Replicas       string `json:"replicas"`
	Node           string `json:"node,omitempty"`
	LastDeployID   int64  `json:"last_deploy_id,omitempty"`
	LastDeployAt   string `json:"last_deploy_at,omitempty"`
	LastDeployStat string `json:"last_deploy_status,omitempty"`

	// The server's diagnostics of the service, each left out when it could
	// not be read. A server older than the agent API diagnostics sends none
	// of them.
	Tasks []TaskStatus `json:"tasks,omitempty"` // most recent first
	// FailedSinceDeploy is a lower bound: Swarm keeps a few tasks per slot.
	FailedSinceDeploy *int         `json:"failed_since_deploy,omitempty"`
	FailedSince       string       `json:"failed_since,omitempty"`
	Health            []TaskHealth `json:"health,omitempty"`
	Usage             []TaskUsage  `json:"usage,omitempty"`
}

// TaskStatus is one Swarm task of the app's service, as `docker service ps`
// shows it.
type TaskStatus struct {
	ID           string `json:"id"`
	Node         string `json:"node,omitempty"`
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
	ExitCode     int    `json:"exit_code"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// TaskHealth is the healthcheck state of one task's container.
type TaskHealth struct {
	TaskID        string `json:"task_id"`
	Node          string `json:"node,omitempty"`
	Status        string `json:"status"` // starting | healthy | unhealthy
	FailingStreak int    `json:"failing_streak"`
	LastExitCode  int    `json:"last_exit_code"`
	LastAt        string `json:"last_at,omitempty"`
}

// TaskUsage is what one task's container uses. CPUPct is absent when the
// sample could not be attributed to this container alone.
type TaskUsage struct {
	TaskID        string   `json:"task_id"`
	Node          string   `json:"node,omitempty"`
	MemBytes      int64    `json:"mem_bytes"`
	MemLimitBytes int64    `json:"mem_limit_bytes"`
	CPUPct        *float64 `json:"cpu_pct,omitempty"`
	CPUSampledAt  string   `json:"cpu_sampled_at,omitempty"`
}

// EnvKey is a variable name and where it comes from. Values are never
// returned by the API.
type EnvKey struct {
	Key    string `json:"key"`
	Source string `json:"source"`
}

// LogLine is one parsed runtime log line.
type LogLine struct {
	Time    string `json:"t"`
	Level   string `json:"lvl"`
	Message string `json:"msg"`
}

// Deployment is one entry of the deploy history. ImageTag records what was
// actually deployed, which for an image app is usually a digest-pinned
// reference rather than the tag that was asked for.
type Deployment struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	Trigger   string `json:"trigger"`
	ImageTag  string `json:"image_tag,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// DeploymentDetail is Deployment plus the tail of its log.
type DeploymentDetail struct {
	Deployment
	LogTail string `json:"log_tail"`
	// Settling: applied, Swarm still converging — still running.
	Settling bool `json:"settling,omitempty"`
}

// Accepted is the answer to an enqueued deploy or rebuild. Status is always
// the literal "running" — it is what the server just wrote, not an
// observation, so it must never be rendered as progress.
type Accepted struct {
	DeploymentID int64  `json:"deployment_id"`
	Status       string `json:"status"`
}

// LogWaitResult answers a logs call that waited for a line (wait_for).
type LogWaitResult struct {
	Found bool `json:"found"`
	// StoppedBy: match, timeout, shutdown, cancelled, ended or error.
	StoppedBy string    `json:"stopped_by"`
	Match     *LogLine  `json:"match,omitempty"`
	Lines     []LogLine `json:"lines"`
	// FollowingSince is when the live read began; earlier lines are not
	// covered. Empty when it never opened.
	FollowingSince string `json:"following_since,omitempty"`
	// Incomplete: the live read broke, so no match is no proof.
	Incomplete bool `json:"incomplete,omitempty"`
}

// AppConfig is an application's saved settings without secrets.
type AppConfig struct {
	ID          int64           `json:"id"`
	Path        string          `json:"path"`
	Source      ConfigSource    `json:"source"`
	Resources   ConfigResources `json:"resources"`
	Healthcheck ConfigHealth    `json:"healthcheck"`
	Port        int32           `json:"port"`
	Ports       []ConfigPort    `json:"ports"`
	Volumes     []ConfigVolume  `json:"volumes"`
	Domains     []ConfigDomain  `json:"domains"`
	Links       []ConfigLink    `json:"links"`
	Pending     ConfigPending   `json:"pending"`
}

// ConfigSource is where the app's image comes from.
type ConfigSource struct {
	Type         string   `json:"type"`
	Image        string   `json:"image,omitempty"`
	Tag          string   `json:"tag,omitempty"`
	Registry     string   `json:"registry,omitempty"`
	GitURL       string   `json:"git_url,omitempty"`
	Branch       string   `json:"git_branch,omitempty"`
	Dockerfile   string   `json:"dockerfile_path,omitempty"`
	BuildArgs    []string `json:"build_args,omitempty"`
	BuildSecrets []string `json:"build_secrets,omitempty"`
}

// ConfigResources is the app's size and placement.
type ConfigResources struct {
	Replicas           int32    `json:"replicas"`
	MemoryLimit        string   `json:"memory_limit,omitempty"`
	MemoryLimitSource  string   `json:"memory_limit_source"`
	CPULimit           string   `json:"cpu_limit,omitempty"`
	CPULimitSource     string   `json:"cpu_limit_source"`
	RestartCondition   string   `json:"restart_condition"`
	RestartMaxAttempts int32    `json:"restart_max_attempts"`
	Placement          string   `json:"placement"`
	PlacementNodes     []string `json:"placement_nodes,omitempty"`
}

// ConfigHealth is the healthcheck's timing; its command is never returned.
type ConfigHealth struct {
	Configured  bool   `json:"configured"`
	Interval    string `json:"interval,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Retries     int32  `json:"retries,omitempty"`
	StartPeriod string `json:"start_period,omitempty"`
}

// ConfigPort is a raw published port.
type ConfigPort struct {
	HostPort      int32  `json:"host_port"`
	ContainerPort int32  `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// ConfigVolume is a named volume and its mount point.
type ConfigVolume struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
}

// ConfigDomain is one domain and how it is routed.
type ConfigDomain struct {
	Host              string   `json:"host"`
	Primary           bool     `json:"primary"`
	Exposed           bool     `json:"exposed"`
	TLS               bool     `json:"tls"`
	Paths             []string `json:"paths,omitempty"`
	RedirectToPrimary bool     `json:"redirect_to_primary"`
	Protection        []string `json:"protection,omitempty"`
}

// ConfigLink is a variable a deploy fills from a database, server or bucket.
type ConfigLink struct {
	Var    string `json:"var"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Server string `json:"server,omitempty"`
	Field  string `json:"field,omitempty"`
}

// ConfigPending lists settings sections saved but not applied yet. Known is
// false when there is nothing to compare with.
type ConfigPending struct {
	Known    bool     `json:"known"`
	Sections []string `json:"sections"`
}

// BackupInfo is one configured backup and its last result.
type BackupInfo struct {
	Kind          string `json:"kind"`
	ID            int64  `json:"id"`
	Target        string `json:"target"`
	Server        string `json:"server,omitempty"`
	Destination   string `json:"destination"`
	Schedule      string `json:"schedule"`
	Enabled       bool   `json:"enabled"`
	Retention     int32  `json:"retention"`
	Running       bool   `json:"running"`
	LastRunAt     string `json:"last_run_at,omitempty"`
	LastStatus    string `json:"last_status,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
}

// BackupStarted answers a started backup run. Its result is the list entry
// whose last_run_at equals StartedAt, once that entry is no longer running.
type BackupStarted struct {
	Kind      string `json:"kind"`
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
}

// DomainStatus is the DNS and certificate state of an app's domains.
type DomainStatus struct {
	App          string        `json:"app"`
	GatewayKnown bool          `json:"gateway_known"`
	Domains      []DomainCheck `json:"domains"`
}

// DomainCheck is one domain's state. PointsAtGateway is absent when the
// gateway's addresses are unknown or DNS failed; Certificate only for TLS.
type DomainCheck struct {
	Host            string     `json:"host"`
	Exposed         bool       `json:"exposed"`
	TLS             bool       `json:"tls"`
	DNS             DNSCheck   `json:"dns"`
	PointsAtGateway *bool      `json:"points_at_gateway,omitempty"`
	Certificate     *CertCheck `json:"certificate,omitempty"`
}

// DNSCheck is what the control plane's resolver answered.
type DNSCheck struct {
	Addresses []string `json:"addresses,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// CertCheck is the certificate the gateway presented for a host.
type CertCheck struct {
	Subject     string `json:"subject,omitempty"`
	Issuer      string `json:"issuer,omitempty"`
	NotBefore   string `json:"not_before,omitempty"`
	NotAfter    string `json:"not_after,omitempty"`
	DaysLeft    int    `json:"days_left"`
	Trusted     bool   `json:"trusted"`
	VerifyError string `json:"verify_error,omitempty"`
	DefaultCert bool   `json:"default_cert"`
	Error       string `json:"error,omitempty"`
}
