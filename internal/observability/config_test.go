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
			got, err := RenderNodeConfig(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, got)
		})
	}
}

// fullNodes is the two-node cluster shape from the live acceptance run: a
// manager whose Krill name is "control-plane" and a worker with a plain name,
// plus a hostname carrying regex metacharacters to exercise escaping.
func fullNodes() []NodeName {
	return []NodeName{
		{Hostname: "node-1.example.com", Name: "control-plane"},
		{Hostname: "worker-1.internal", Name: "worker-1"},
	}
}

func TestRenderNodeConfigWithNodesGolden(t *testing.T) {
	got, err := RenderNodeConfig(fullSettings(), fullNodes())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "node_with_nodes.alloy", got)
}

// TestRenderNodeConfigNodesSorted proves the rendered rule order depends only
// on Hostname, not on the order nodes were resolved in — otherwise the config
// (and so the object hash and the redeploy it triggers) would flap between
// reconcile passes that resolve the same nodes in a different order.
func TestRenderNodeConfigNodesSorted(t *testing.T) {
	forward := []NodeName{{Hostname: "a-node", Name: "a"}, {Hostname: "b-node", Name: "b"}}
	backward := []NodeName{{Hostname: "b-node", Name: "b"}, {Hostname: "a-node", Name: "a"}}
	got1, err := RenderNodeConfig(fullSettings(), forward)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := RenderNodeConfig(fullSettings(), backward)
	if err != nil {
		t.Fatal(err)
	}
	if string(got1) != string(got2) {
		t.Errorf("node order changed the rendered config:\n--- forward ---\n%s\n--- backward ---\n%s", got1, got2)
	}
	ai := strings.Index(string(got1), `regex         = "a-node"`)
	bi := strings.Index(string(got1), `regex         = "b-node"`)
	if ai < 0 || bi < 0 || ai > bi {
		t.Errorf("rules not sorted by hostname: a-node at %d, b-node at %d", ai, bi)
	}
}

// TestRenderNodeConfigNodesSkipsIncomplete covers entries with an empty
// Hostname or Name: an unmapped node must keep its raw hostname rather than
// get a bogus rule, so it is simply omitted.
func TestRenderNodeConfigNodesSkipsIncomplete(t *testing.T) {
	got, err := RenderNodeConfig(fullSettings(), []NodeName{{Hostname: "", Name: "x"}, {Hostname: "y", Name: ""}})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "node_full.alloy", got) // identical to no nodes at all
}

// TestRenderNodeConfigNodesRejectUnsafe proves a node name or hostname that
// cannot go into the configuration as a string literal fails the same way an
// unsafe URL/user does, rather than silently breaking out of a string.
func TestRenderNodeConfigNodesRejectUnsafe(t *testing.T) {
	for _, nodes := range [][]NodeName{
		{{Hostname: "bad\"host", Name: "x"}},
		{{Hostname: "host", Name: "bad\\name"}},
		{{Hostname: "host with space", Name: "x"}},
	} {
		if _, err := RenderNodeConfig(fullSettings(), nodes); !errors.Is(err, errUnsafeText) {
			t.Errorf("RenderNodeConfig(nodes=%+v) = %v, want errUnsafeText", nodes, err)
		}
	}
}

// TestAlloyRegexLiteralEscapesForRiver pins the exact escaping RenderNodeConfig
// relies on: regexp.QuoteMeta alone is not enough because Alloy's River syntax
// treats a bare backslash in a string literal as introducing an escape
// sequence — "node-1\.ru" fails to load with "unknown escape sequence" even
// though `alloy validate` accepts it (verified manually against the pinned
// grafana/alloy:v1.19.2 image's /api/v0/web/components introspection endpoint,
// which echoed back a raw Go string value with a real single backslash before
// each escaped dot, produced only by the doubled-backslash source form).
func TestAlloyRegexLiteralEscapesForRiver(t *testing.T) {
	got := alloyRegexLiteral("node-1.example.com")
	want := `"node-1\\.example\\.com"`
	if got != want {
		t.Errorf("alloyRegexLiteral = %s, want %s", got, want)
	}
}

func TestRenderNodeConfigContents(t *testing.T) {
	got, err := RenderNodeConfig(fullSettings(), nil)
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

	noPass, _ := RenderNodeConfig(Settings{Metrics: Target{URL: "https://m/x", User: "u"}}, nil)
	if !strings.Contains(string(noPass), `username = "u"`) || strings.Contains(string(noPass), "password_file") {
		t.Errorf("user without password:\n%s", noPass)
	}
	metricsOnly, _ := RenderNodeConfig(Settings{Metrics: Target{URL: "https://m/x"}}, nil)
	if strings.Contains(string(metricsOnly), "loki") || strings.Contains(string(metricsOnly), "basic_auth") {
		t.Errorf("metrics-only config:\n%s", metricsOnly)
	}
}

func TestRenderNodeConfigRejects(t *testing.T) {
	if _, err := RenderNodeConfig(Settings{Enabled: true}, nil); !errors.Is(err, ErrNothingConfigured) {
		t.Errorf("empty settings: %v", err)
	}
	// Values that bypassed Save must still not break out of a string literal.
	for _, s := range []Settings{
		{Metrics: Target{URL: "https://m/x\"\n}"}},
		{Logs: Target{URL: "https://l/x", User: "a\\\"b"}},
	} {
		if _, err := RenderNodeConfig(s, nil); err == nil {
			t.Errorf("RenderNodeConfig(%+v) accepted unsafe text", s)
		}
	}
}
