package observability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/proshik/krill/internal/docker"
)

const (
	// Image is the agent image, pinned by digest; a Krill release moves it.
	Image           = "grafana/alloy:v1.19.2@sha256:b8ec653c44235fbe910879145dac3597d66b0aaecf60bcbbe82580767771a839"
	NodeServiceName = "krill-alloy-node"
	nodeVolume      = "krill-alloy-node-data"
	specHashLabel   = "krill.alloy.spec-hash"

	// ObjectLabel marks the Swarm configs and secrets Krill owns for the agent,
	// so a prune never touches anything else.
	ObjectLabel      = "krill.observability"
	ObjectLabelValue = "node-agent"

	// The agent's limits. KRILL_DEFAULT_MEMORY_LIMIT is sized for apps and is
	// too generous for a per-node agent on a small server; Alloy derives
	// GOMEMLIMIT from the cgroup limit by itself.
	NodeMemoryLimit = 256 << 20
	NodeNanoCPUs    = 500_000_000
)

// Objects names the Swarm configs and secrets one agent spec mounts. "" means
// the secret is not needed.
type Objects struct {
	Config        string
	MetricsSecret string
	LogsSecret    string
}

// objectName derives an immutable object's name from its content: a changed
// value is a new object, which is what makes Swarm roll the service.
// The 32-bit fingerprint of a password tells nothing to anyone who can list
// secrets: that takes the docker socket, which can read the secret anyway.
func objectName(prefix string, data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

// objectsOf names the objects for settings s and rendered configuration cfg.
func objectsOf(s Settings, cfg []byte) Objects {
	obj := Objects{Config: objectName("krill-alloy-node", cfg)}
	if s.Metrics.Configured() && s.Metrics.User != "" && s.Metrics.Password != "" {
		obj.MetricsSecret = objectName("krill-alloy-metrics-password", []byte(s.Metrics.Password))
	}
	if s.Logs.Configured() && s.Logs.User != "" && s.Logs.Password != "" {
		obj.LogsSecret = objectName("krill-alloy-logs-password", []byte(s.Logs.Password))
	}
	return obj
}

// NodeSpec is the global agent service. It sits on the base network only (for
// egress) and never on an organization's network: a container holding the
// docker socket stays out of the networks tenant code runs in. Tenant services
// the per-organization network migration has not moved yet still share the
// base network; the agent listens on nothing reachable, so that exposes nothing.
func NodeSpec(obj Objects, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:     NodeServiceName,
		Image:    Image,
		Args:     nodeArgs(),
		Global:   true,
		Network:  network,
		Hostname: "{{.Node.Hostname}}",
		Mounts: []docker.MountSpec{
			{Source: "/proc", Target: "/host/proc", ReadOnly: true},
			{Source: "/sys", Target: "/host/sys", ReadOnly: true},
			{Source: "/", Target: "/host/root", ReadOnly: true},
			{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true},
			{Type: "volume", Source: nodeVolume, Target: storagePath},
		},
		Configs:          []docker.FileRef{{Name: obj.Config, Target: configPath}},
		MemoryLimitBytes: NodeMemoryLimit,
		NanoCPUs:         NodeNanoCPUs,
		// Two agents on one node would share the WAL volume.
		UpdateStopFirst: true,
	}
	if obj.MetricsSecret != "" {
		spec.Secrets = append(spec.Secrets, docker.FileRef{Name: obj.MetricsSecret, Target: metricsSecretFile, Mode: 0o400})
	}
	if obj.LogsSecret != "" {
		spec.Secrets = append(spec.Secrets, docker.FileRef{Name: obj.LogsSecret, Target: logsSecretFile, Mode: 0o400})
	}
	spec.Labels = map[string]string{specHashLabel: fingerprint(spec)}
	return spec
}

// fingerprint hashes the whole spec; "" never matches a stored label.
func fingerprint(s docker.ServiceSpec) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
