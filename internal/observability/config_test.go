package observability

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func fullSettings() Settings {
	return Settings{
		Enabled: true,
		Metrics: Target{URL: "https://mimir.example.com/api/v1/push", User: "tenant-1", Password: "mpw"},
		Logs:    Target{URL: "https://loki.example.com/loki/api/v1/push", User: "loki", Password: "lpw"},
	}
}

func TestRenderNodeConfigGolden(t *testing.T) {
	cases := map[string]Settings{
		"node_full.alloy":           fullSettings(),
		"node_metrics_noauth.alloy": {Enabled: true, Metrics: Target{URL: "http://prometheus:9090/api/v1/write"}},
		"node_logs_only.alloy":      {Enabled: true, Logs: Target{URL: "http://loki:3100/loki/api/v1/push", User: "u"}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := RenderNodeConfig(s)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, got)
		})
	}
}

func TestRenderNodeConfigContents(t *testing.T) {
	got, err := RenderNodeConfig(fullSettings())
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(got)
	for _, want := range []string{
		`password_file = "/run/secrets/krill_metrics_password"`,
		`password_file = "/run/secrets/krill_logs_password"`,
		`protobuf_message = "prometheus.WriteRequest"`,
		`udev_data_path = "/host/root/run/udev/data"`,
		`source_labels = ["__meta_docker_container_label_krill_app_id"]`,
		`target_label  = "krill_service"`,
		`labels        = {krill_node = constants.hostname}`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	for _, secret := range []string{"mpw", "lpw"} {
		if strings.Contains(cfg, secret) {
			t.Errorf("config contains the password %q", secret)
		}
	}

	noPass, _ := RenderNodeConfig(Settings{Metrics: Target{URL: "https://m/x", User: "u"}})
	if !strings.Contains(string(noPass), `username = "u"`) || strings.Contains(string(noPass), "password_file") {
		t.Errorf("user without password:\n%s", noPass)
	}
	metricsOnly, _ := RenderNodeConfig(Settings{Metrics: Target{URL: "https://m/x"}})
	if strings.Contains(string(metricsOnly), "loki") || strings.Contains(string(metricsOnly), "basic_auth") {
		t.Errorf("metrics-only config:\n%s", metricsOnly)
	}
}

func TestRenderNodeConfigRejects(t *testing.T) {
	if _, err := RenderNodeConfig(Settings{Enabled: true}); !errors.Is(err, ErrNothingConfigured) {
		t.Errorf("empty settings: %v", err)
	}
	// Values that bypassed Save must still not break out of a string literal.
	for _, s := range []Settings{
		{Metrics: Target{URL: "https://m/x\"\n}"}},
		{Logs: Target{URL: "https://l/x", User: "a\\\"b"}},
	} {
		if _, err := RenderNodeConfig(s); err == nil {
			t.Errorf("RenderNodeConfig(%+v) accepted unsafe text", s)
		}
	}
}
