package config

import "github.com/caarlos0/env/v11"

// Config holds the Krill configuration from environment variables.
type Config struct {
	ListenAddr    string `env:"KRILL_LISTEN_ADDR" envDefault:":8080"`
	DatabaseURL   string `env:"KRILL_DATABASE_URL,required"`
	AdminEmail    string `env:"KRILL_ADMIN_EMAIL,required"`
	AdminPassword string `env:"KRILL_ADMIN_PASSWORD,required"`
	DockerHost    string `env:"KRILL_DOCKER_HOST"`
	BaseDomain    string `env:"KRILL_BASE_DOMAIN" envDefault:"127-0-0-1.sslip.io"`
	Network       string `env:"KRILL_NETWORK" envDefault:"krill-net"`
	Host          string `env:"KRILL_HOST" envDefault:"localhost"`
	CookieSecure  bool   `env:"KRILL_COOKIE_SECURE" envDefault:"false"`
	LogLevel      string `env:"KRILL_LOG_LEVEL" envDefault:"info"`   // debug | info | warn | error
	LogFormat     string `env:"KRILL_LOG_FORMAT" envDefault:"text"`  // text | json
	AcmeEmail     string `env:"KRILL_ACME_EMAIL"`
	AcmeStaging   bool   `env:"KRILL_ACME_STAGING" envDefault:"false"`
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}
