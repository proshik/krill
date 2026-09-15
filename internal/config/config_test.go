package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("KRILL_DATABASE_URL", "postgres://x")
	t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
	t.Setenv("KRILL_ADMIN_PASSWORD", "pw")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr default = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.BaseDomain != "127-0-0-1.sslip.io" {
		t.Errorf("BaseDomain default = %q", cfg.BaseDomain)
	}
	if cfg.Network != "krill-net" {
		t.Errorf("Network default = %q", cfg.Network)
	}
	if cfg.CookieSecure {
		t.Errorf("CookieSecure default = true, want false")
	}
	if cfg.Host != "localhost" {
		t.Errorf("Host default = %q, want localhost", cfg.Host)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("KRILL_DATABASE_URL", "postgres://x")
	t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
	t.Setenv("KRILL_ADMIN_PASSWORD", "pw")
	t.Setenv("KRILL_LISTEN_ADDR", ":9000")
	t.Setenv("KRILL_BASE_DOMAIN", "example.test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":9000" || cfg.BaseDomain != "example.test" {
		t.Errorf("override failed: %+v", cfg)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
	t.Setenv("KRILL_ADMIN_PASSWORD", "pw")
	// KRILL_DATABASE_URL is not set
	if _, err := Load(); err == nil {
		t.Fatal("expected error when KRILL_DATABASE_URL is missing")
	}
}

func TestAcmeDefaults(t *testing.T) {
	t.Setenv("KRILL_DATABASE_URL", "postgres://x")
	t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
	t.Setenv("KRILL_ADMIN_PASSWORD", "pw")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AcmeStaging != false || c.AcmeEmail != "" {
		t.Errorf("acme defaults wrong: staging=%v email=%q", c.AcmeStaging, c.AcmeEmail)
	}
}

func TestUpdateConfigDefaults(t *testing.T) {
	t.Setenv("KRILL_DATABASE_URL", "postgres://x")
	t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
	t.Setenv("KRILL_ADMIN_PASSWORD", "pw")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpdateCheckInterval != 24*time.Hour {
		t.Errorf("UpdateCheckInterval default = %v, want 24h", cfg.UpdateCheckInterval)
	}
	if cfg.UpdateRepo != "proshik/krill" {
		t.Errorf("UpdateRepo default = %q, want proshik/krill", cfg.UpdateRepo)
	}
}

func TestUpdateCheckIntervalZeroDisables(t *testing.T) {
	t.Setenv("KRILL_DATABASE_URL", "postgres://x")
	t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
	t.Setenv("KRILL_ADMIN_PASSWORD", "pw")
	t.Setenv("KRILL_UPDATE_CHECK_INTERVAL", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpdateCheckInterval != 0 {
		t.Errorf("UpdateCheckInterval = %v, want 0", cfg.UpdateCheckInterval)
	}
}

func TestUpdateRepoValidation(t *testing.T) {
	cases := []struct {
		name    string
		repo    string
		wantErr bool
	}{
		{"valid fork", "someone/krill-fork", false},
		{"no slash", "bad", true},
		{"too many segments", "a/b/c", true},
		{"parent traversal in owner", "../x", true},
		{"parent traversal in name", "a/..", true},
		{"space", "a b/c", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KRILL_DATABASE_URL", "postgres://x")
			t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
			t.Setenv("KRILL_ADMIN_PASSWORD", "pw")
			t.Setenv("KRILL_UPDATE_REPO", tc.repo)

			_, err := Load()
			if tc.wantErr && err == nil {
				t.Fatalf("Load() with KRILL_UPDATE_REPO=%q: expected error, got nil", tc.repo)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Load() with KRILL_UPDATE_REPO=%q: unexpected error: %v", tc.repo, err)
			}
		})
	}
}

// caarlos0/env treats an explicitly-set-to-empty variable the same as unset
// when the field carries an envDefault (see getOr in env.go): the default
// wins rather than surfacing as an empty string. So an explicitly empty
// KRILL_UPDATE_REPO can never reach the owner/name validation as "" — it
// resolves to the default and Load succeeds, exactly like every other
// envDefault-carrying field in this package (e.g. KRILL_NETWORK="").
func TestUpdateRepoExplicitlyEmptyFallsBackToDefault(t *testing.T) {
	t.Setenv("KRILL_DATABASE_URL", "postgres://x")
	t.Setenv("KRILL_ADMIN_EMAIL", "a@b.c")
	t.Setenv("KRILL_ADMIN_PASSWORD", "pw")
	t.Setenv("KRILL_UPDATE_REPO", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpdateRepo != "proshik/krill" {
		t.Errorf("UpdateRepo = %q, want default proshik/krill", cfg.UpdateRepo)
	}
}

func TestBaseURL(t *testing.T) {
	cases := []struct {
		name string
		c    Config
		want string
	}{
		{"explicit override", Config{PublicURL: "https://krill.example.com", Host: "ignored", CookieSecure: false}, "https://krill.example.com"},
		{"https from cookie_secure", Config{Host: "krill.example.net", CookieSecure: true}, "https://krill.example.net"},
		{"http default", Config{Host: "localhost", CookieSecure: false}, "http://localhost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.BaseURL(); got != tc.want {
				t.Fatalf("BaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
