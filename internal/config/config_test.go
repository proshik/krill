package config

import "testing"

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
	// KRILL_DATABASE_URL не задан
	if _, err := Load(); err == nil {
		t.Fatal("expected error when KRILL_DATABASE_URL is missing")
	}
}
