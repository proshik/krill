package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// updateRepoPattern matches a GitHub "owner/name" repository slug.
var updateRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Config holds the Krill configuration from environment variables.
type Config struct {
	ListenAddr    string `env:"KRILL_LISTEN_ADDR" envDefault:":8080"`
	DatabaseURL   string `env:"KRILL_DATABASE_URL,required"`
	AdminEmail    string `env:"KRILL_ADMIN_EMAIL,required"`
	AdminPassword string `env:"KRILL_ADMIN_PASSWORD,required"`
	DockerHost    string `env:"KRILL_DOCKER_HOST"`
	// AdvertiseAddr is the manager IP/host a worker dials to join the swarm
	// (":2377" is appended). Required to add worker nodes; empty disables it.
	AdvertiseAddr string `env:"KRILL_ADVERTISE_ADDR"`
	BaseDomain    string `env:"KRILL_BASE_DOMAIN" envDefault:"127-0-0-1.sslip.io"`
	Network       string `env:"KRILL_NETWORK" envDefault:"krill-net"`
	Host          string `env:"KRILL_HOST" envDefault:"localhost"`
	PublicURL     string `env:"KRILL_PUBLIC_URL"`
	CookieSecure  bool   `env:"KRILL_COOKIE_SECURE" envDefault:"false"`
	// TrustProxy: honour X-Forwarded-For when deriving the client IP (login
	// rate limiting). Enable ONLY when a reverse proxy fronts Krill and
	// overwrites/appends the header — otherwise any client can forge it and
	// hand itself a private rate-limit bucket. Off by default, matching the
	// installer's direct-on-host deployment.
	TrustProxy  bool   `env:"KRILL_TRUST_PROXY" envDefault:"false"`
	LogLevel    string `env:"KRILL_LOG_LEVEL" envDefault:"info"`  // debug | info | warn | error
	LogFormat   string `env:"KRILL_LOG_FORMAT" envDefault:"text"` // text | json
	AcmeEmail   string `env:"KRILL_ACME_EMAIL"`
	AcmeStaging bool   `env:"KRILL_ACME_STAGING" envDefault:"false"`
	// SecretKey enables encryption-at-rest of stored secrets (DB passwords,
	// registry/destination credentials). Any string; hashed to a 32-byte AES key.
	// Empty = secrets stored as plaintext (legacy, logged as a warning).
	SecretKey string `env:"KRILL_SECRET_KEY"`
	// ConvergeTimeout caps how long a deploy waits for the service to become
	// healthy before giving up (auto-extended by a healthcheck's start_period).
	ConvergeTimeout time.Duration `env:"KRILL_CONVERGE_TIMEOUT" envDefault:"180s"`
	// MigrateTimeout bounds a DB-instance volume migration (stop → copy →
	// redeploy). Large volumes or slow links may need more.
	MigrateTimeout time.Duration `env:"KRILL_MIGRATE_TIMEOUT" envDefault:"30m"`
	// HealthPollInterval is how often the notification health watcher polls
	// service state to detect apps going down / recovering.
	HealthPollInterval time.Duration `env:"KRILL_HEALTH_POLL_INTERVAL" envDefault:"30s"`
	// TerminalIdleTimeout closes an interactive web-terminal session after this
	// long with no I/O (reaps abandoned shells). 0 disables the idle timeout.
	TerminalIdleTimeout time.Duration `env:"KRILL_TERMINAL_IDLE_TIMEOUT" envDefault:"15m"`
	// MetricsInterval is how often the monitoring sampler records container stats.
	MetricsInterval time.Duration `env:"KRILL_METRICS_INTERVAL" envDefault:"30s"`
	// MetricsRetention is how long metric history is kept before pruning.
	MetricsRetention time.Duration `env:"KRILL_METRICS_RETENTION" envDefault:"48h"`
	// MetricsNodeTimeout caps how long the sampler waits per worker node.
	MetricsNodeTimeout time.Duration `env:"KRILL_METRICS_NODE_TIMEOUT" envDefault:"10s"`
	// DefaultMemoryLimit / DefaultCPULimit are the instance-wide resource limits
	// applied to any app or DB instance that has no explicit value of its own —
	// an unlimited container can take the whole host and starve every other
	// tenant, so "no limit" is not a safe default. Human units, parsed by
	// docker.ParseMemoryBytes / docker.ParseNanoCPUs ("512m", "1.0"). Empty
	// means "no default" for that resource; the Advanced tab still overrides
	// these per app.
	DefaultMemoryLimit string `env:"KRILL_DEFAULT_MEMORY_LIMIT" envDefault:"512m"`
	DefaultCPULimit    string `env:"KRILL_DEFAULT_CPU_LIMIT" envDefault:"1.0"`
	// AllowPrivateEgress disables the SSRF egress guard (internal/netguard) for
	// S3 destination/backup traffic and registry HTTP calls, allowing outbound
	// connections to private/loopback/link-local addresses. Default false.
	AllowPrivateEgress bool `env:"KRILL_ALLOW_PRIVATE_EGRESS" envDefault:"false"`
	// AgentAPIEnabled toggles the agent-facing API (REST /api/v1 + MCP /mcp). On by
	// default; set false to disable the surface entirely on an install that
	// doesn't want it.
	AgentAPIEnabled bool `env:"KRILL_AGENT_API_ENABLED" envDefault:"true"`
	// MCPSessionTimeout closes an idle MCP session after this long with no
	// request from its client, releasing the session's goroutine and its slot
	// in the handler's session table. An MCP session is only ever ended
	// explicitly by a DELETE /mcp, which an agent that crashes, a CI job that
	// finishes, a restarted container or a dropped network never sends — so
	// without this every abandoned connect leaks until the process restarts,
	// and Krill runs for months as a systemd unit. 0 disables the idle
	// timeout (sessions then live until an explicit DELETE), matching
	// KRILL_TERMINAL_IDLE_TIMEOUT's convention.
	MCPSessionTimeout time.Duration `env:"KRILL_MCP_SESSION_TIMEOUT" envDefault:"30m"`
	// MaxBuildsPerOrg caps how many deploys one organization may have in flight
	// on the shared build queue at once. The queue is one worker shared by every
	// tenant, so without this an org with many apps could hold it indefinitely
	// and stall every other tenant's deploys. <= 0 disables the cap.
	MaxBuildsPerOrg int `env:"KRILL_MAX_BUILDS_PER_ORG" envDefault:"2"`
	// BuildCacheLimit is the --reserved-space value (--keep-storage on a Docker
	// CLI older than 28) passed to `docker builder prune` on each
	// BuildPruneInterval tick. Nothing else ever trims the
	// BuildKit cache, so on a small VPS the disk fills silently until builds
	// start failing. Empty disables pruning.
	BuildCacheLimit string `env:"KRILL_BUILD_CACHE_LIMIT" envDefault:"5gb"`
	// BuildPruneInterval is how often the BuildKit cache is pruned. <= 0
	// disables pruning.
	BuildPruneInterval time.Duration `env:"KRILL_BUILD_PRUNE_INTERVAL" envDefault:"24h"`
	// UpdateCheckInterval is how often Krill checks GitHub for a newer release.
	// <= 0 disables the background check; the manual "Check now" still works.
	UpdateCheckInterval time.Duration `env:"KRILL_UPDATE_CHECK_INTERVAL" envDefault:"24h"`
	// UpdateRepo is the GitHub repository ("owner/name") releases are read from.
	// A fork is useful for acceptance tests of the self-update.
	UpdateRepo string `env:"KRILL_UPDATE_REPO" envDefault:"proshik/krill"`
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, err
	}
	if !updateRepoPattern.MatchString(c.UpdateRepo) {
		return Config{}, fmt.Errorf("KRILL_UPDATE_REPO %q: want owner/name", c.UpdateRepo)
	}
	owner, name, _ := strings.Cut(c.UpdateRepo, "/")
	if owner == "." || owner == ".." || name == "." || name == ".." {
		return Config{}, fmt.Errorf("KRILL_UPDATE_REPO %q: want owner/name", c.UpdateRepo)
	}
	return c, nil
}

// BaseURL is the externally reachable base URL used to display webhook URLs.
// KRILL_PUBLIC_URL wins; otherwise it is derived from KRILL_HOST and the cookie
// security setting (a reverse proxy terminating TLS implies COOKIE_SECURE=true).
func (c Config) BaseURL() string {
	if c.PublicURL != "" {
		return c.PublicURL
	}
	scheme := "http"
	if c.CookieSecure {
		scheme = "https"
	}
	return scheme + "://" + c.Host
}

// AcmeContact is the Let's Encrypt contact address: the explicit
// KRILL_ACME_EMAIL, falling back to the admin's address. Traefik's spec is
// fingerprinted for reconciliation, so every caller has to derive the contact
// the same way — a second, slightly different fallback would make the gateway
// flap between two specs.
func (c Config) AcmeContact() string {
	if c.AcmeEmail != "" {
		return c.AcmeEmail
	}
	return c.AdminEmail
}
