package nav

import (
	"context"
	"testing"
)

func TestSettingsSections(t *testing.T) {
	cases := []struct {
		path    string
		section string
	}{
		{"/orgs/1/panel-domain", "panel-domain"},
		{"/orgs/1/panel-domain/confirm", "panel-domain"},
		{"/orgs/1/updates", "updates"},
		{"/orgs/1/updates/status", "updates"},
		{"/orgs/1/registries", "registries"},
		{"/orgs/1/firewall", "firewall"},
	}
	for _, c := range cases {
		ctx := WithPath(context.Background(), c.path)
		if !IsActive(ctx, c.section) {
			t.Errorf("IsActive(%q, %q) = false, want true", c.path, c.section)
		}
		if !IsSettings(ctx) {
			t.Errorf("IsSettings(%q) = false, want true", c.path)
		}
		if IsActive(ctx, "projects") {
			t.Errorf("IsActive(%q, projects) = true, want false", c.path)
		}
	}
}

func TestNotSettingsSections(t *testing.T) {
	for _, path := range []string{"/orgs/1", "/orgs/1/projects/2", "/orgs/1/members", "/orgs/1/monitoring"} {
		if IsSettings(WithPath(context.Background(), path)) {
			t.Errorf("IsSettings(%q) = true, want false", path)
		}
	}
}

func TestUpdateAvailable(t *testing.T) {
	ctx := context.Background()
	if got := UpdateAvailable(ctx); got != "" {
		t.Errorf("UpdateAvailable(empty ctx) = %q, want \"\"", got)
	}
	ctx = WithUpdateAvailable(ctx, "v0.2.0")
	if got := UpdateAvailable(ctx); got != "v0.2.0" {
		t.Errorf("UpdateAvailable() = %q, want v0.2.0", got)
	}
	// The badge state and the request path live under separate keys.
	ctx = WithPath(ctx, "/orgs/1/registries")
	if got := UpdateAvailable(ctx); got != "v0.2.0" {
		t.Errorf("UpdateAvailable() after WithPath = %q, want v0.2.0", got)
	}
	if got := Path(ctx); got != "/orgs/1/registries" {
		t.Errorf("Path() = %q, want /orgs/1/registries", got)
	}
}
