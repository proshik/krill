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
}

// Accepted is the answer to an enqueued deploy or rebuild. Status is always
// the literal "running" — it is what the server just wrote, not an
// observation, so it must never be rendered as progress.
type Accepted struct {
	DeploymentID int64  `json:"deployment_id"`
	Status       string `json:"status"`
}
