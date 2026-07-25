package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

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
	LogLevel      string `env:"KRILL_LOG_LEVEL" envDefault:"info"`   // debug | info | warn | error
	LogFormat     string `env:"KRILL_LOG_FORMAT" envDefault:"text"`  // text | json
	AcmeEmail     string `env:"KRILL_ACME_EMAIL"`
	AcmeStaging   bool   `env:"KRILL_ACME_STAGING" envDefault:"false"`
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
	// AllowPrivateEgress disables the SSRF egress guard (internal/netguard) for
	// S3 destination/backup traffic and registry HTTP calls, allowing outbound
	// connections to private/loopback/link-local addresses. Default false.
	AllowPrivateEgress bool `env:"KRILL_ALLOW_PRIVATE_EGRESS" envDefault:"false"`
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, err
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
