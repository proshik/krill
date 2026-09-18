package observability

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
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

// TestRenderNodeConfigNodesSortedTiebreak covers two entries sharing the same
// Hostname (a pathological duplicate, but not one nodeRules rejects): without
// a tiebreak, sort.Slice's comparator would see them as "equal" and an
// unstable sort could place them in either order from one reconcile pass to
// the next, changing the rendered config (and so the object hash) for no
// real reason. The tiebreak on Name pins a single deterministic order.
func TestRenderNodeConfigNodesSortedTiebreak(t *testing.T) {
	forward := []NodeName{{Hostname: "same-host", Name: "a-name"}, {Hostname: "same-host", Name: "b-name"}}
	backward := []NodeName{{Hostname: "same-host", Name: "b-name"}, {Hostname: "same-host", Name: "a-name"}}
	got1, err := RenderNodeConfig(fullSettings(), forward)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := RenderNodeConfig(fullSettings(), backward)
	if err != nil {
		t.Fatal(err)
	}
	if string(got1) != string(got2) {
		t.Errorf("input order changed the rendered config for a same-hostname tie:\n--- forward ---\n%s\n--- backward ---\n%s", got1, got2)
	}
	ai := strings.Index(string(got1), `replacement   = "a-name"`)
	bi := strings.Index(string(got1), `replacement   = "b-name"`)
	if ai < 0 || bi < 0 || ai > bi {
		t.Errorf("tie not broken by Name: a-name at %d, b-name at %d", ai, bi)
	}
}

// decodeAlloyStringLiteral strips the surrounding quotes RenderNodeConfig's
// helpers always add. Neither alloyRegexLiteral's nor alloyReplacementLiteral's
// output can contain an unescaped quote (safeText/SafeNodeName forbid one, and
// regexp.QuoteMeta escapes the regex metacharacters, none of which is a
// quote), so a plain strip is exact — no need for a real River lexer here.
func decodeAlloyStringLiteral(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`)
}

// applyRelabelRules reproduces exactly what Alloy's (and the Prometheus
// relabel package it's built on) "replace" action does for a sequence of
// rules, each with a single source label and no explicit action (the
// implicit default is "replace"): compile the regex anchored full-string
// (Prometheus always wraps a relabel regex as "^(?:pattern)$"), and on a
// match set target to regexp's own ExpandString of replacement against the
// match — the same primitives (Go's regexp package) Alloy's relabel
// component itself uses, so this is not a guess at Alloy's behavior, it's
// the same algorithm. source is a fixed map read fresh for every rule: a
// rule that has already run and mutated the same map given as source would
// reproduce the chaining bug this simulates (see below).
type relabelRule struct{ sourceLabel, regex, targetLabel, replacement string }

func applyRelabelRules(rules []relabelRule, source map[string]string) map[string]string {
	labels := make(map[string]string, len(source))
	for k, v := range source {
		labels[k] = v
	}
	for _, r := range rules {
		re := regexp.MustCompile("^(?:" + r.regex + ")$")
		val := labels[r.sourceLabel]
		if m := re.FindStringSubmatchIndex(val); m != nil {
			labels[r.targetLabel] = string(re.ExpandString(nil, r.replacement, val, m))
		}
	}
	return labels
}

// TestNodeRulesDoNotChainOnNameHostnameCollision is the semantic version of
// the structural guard below: it reproduces the exact scenario the "__tmp_krill_node"
// snapshot rule exists for — one node's chosen Name collides with another
// node's raw Swarm hostname — and proves the CORRECT (snapshot-based) rule
// wiring resolves it, while confirming (as a sanity check on the test itself)
// that the naive wiring nodeRules used before this fix — reading AND writing
// "krill_node" in every rule — really would have produced the wrong node
// name, so this test is not vacuously true.
func TestNodeRulesDoNotChainOnNameHostnameCollision(t *testing.T) {
	// Node A's own Name equals node B's raw hostname; A's hostname sorts
	// before B's, so — in a naive read-and-write-the-same-label chain — A's
	// rule runs first and rewrites the shared label to a value that B's rule
	// (running next) then matches and overwrites again, even on an agent that
	// is actually running on node A itself.
	nodes := []NodeName{
		{Hostname: "aaa-host", Name: "zzz-collide"},     // node A
		{Hostname: "zzz-collide", Name: "b-final-name"}, // node B: hostname == A's Name
	}
	rules := nodeRules(nodes)
	if len(rules) != 2 {
		t.Fatalf("nodeRules returned %d rules, want 2", len(rules))
	}
	const agentHostname = "aaa-host" // this agent runs on node A

	naive := make([]relabelRule, len(rules))
	fixed := make([]relabelRule, len(rules))
	for i, r := range rules {
		regex := decodeAlloyStringLiteral(strings.ReplaceAll(r.Regex, `\\`, `\`))
		replacement := decodeAlloyStringLiteral(r.Replacement)
		naive[i] = relabelRule{sourceLabel: "krill_node", regex: regex, targetLabel: "krill_node", replacement: replacement}
		fixed[i] = relabelRule{sourceLabel: "__tmp_krill_node", regex: regex, targetLabel: "krill_node", replacement: replacement}
	}

	// Sanity check: the naive (pre-fix) wiring reproduces the bug. If it
	// didn't, this test would not actually be exercising the collision.
	naiveResult := applyRelabelRules(naive, map[string]string{"krill_node": agentHostname})
	if naiveResult["krill_node"] != "b-final-name" {
		t.Fatalf("sanity check failed: the naive simulation did not reproduce the chaining bug (got %q, want %q) — this test is not exercising the collision",
			naiveResult["krill_node"], "b-final-name")
	}

	fixedResult := applyRelabelRules(fixed, map[string]string{"krill_node": agentHostname, "__tmp_krill_node": agentHostname})
	if got := fixedResult["krill_node"]; got != "zzz-collide" {
		t.Errorf("krill_node = %q, want %q (node A's own name, unaffected by node B's rule)", got, "zzz-collide")
	}
}

// TestRenderNodeConfigNodesUseImmutableSnapshot is a regression guard for the
// rule-chaining fix at the template-text level: every per-node relabel rule
// must read from the "__tmp_krill_node" snapshot, never from "krill_node"
// itself — reading from "krill_node" is exactly what let one node's rule see
// another's already-rewritten value (see
// TestNodeRulesDoNotChainOnNameHostnameCollision).
func TestRenderNodeConfigNodesUseImmutableSnapshot(t *testing.T) {
	got, err := RenderNodeConfig(fullSettings(), fullNodes())
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(got)
	if strings.Contains(cfg, `source_labels = ["krill_node"]`) {
		t.Errorf("a per-node rule reads from the mutable \"krill_node\" label instead of the immutable snapshot:\n%s", cfg)
	}
	want := len(fullNodes()) * 2 // once per node, once for metrics + once for logs
	if got := strings.Count(cfg, `source_labels = ["__tmp_krill_node"]`); got != want {
		t.Errorf(`source_labels = ["__tmp_krill_node"] appears %d times, want %d`, got, want)
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

// TestRenderNodeConfigNodesSkipsUnsafe covers a node hostname/name that
// cannot go into the configuration as a string literal — a quote, a
// backslash, or (for the name specifically) a control character; a space is
// NOT one of these (see TestRenderNodeConfigNodesAllowsSpaceAndUnicode) since
// node_labels.label allows it and it is harmless inside an Alloy string
// literal. Failing the whole render would freeze a running agent on its last
// config (or leave a disabled one undeployed) because of one badly-named
// node, and the resulting error has nowhere good to surface (RenderNodeConfig
// has no request/response to attach a "which node" detail to — it would
// resurface as the observability settings page's LastErr, which is about the
// push addresses, not the cluster). So the entry is skipped instead — it
// keeps its raw hostname label, like an unmapped node — the render succeeds,
// and the other nodes are still present.
func TestRenderNodeConfigNodesSkipsUnsafe(t *testing.T) {
	for _, nodes := range [][]NodeName{
		{{Hostname: "bad\"host", Name: "x"}, {Hostname: "good-host", Name: "good-name"}},
		{{Hostname: "host", Name: "bad\\name"}, {Hostname: "good-host", Name: "good-name"}},
		{{Hostname: "host with space", Name: "x"}, {Hostname: "good-host", Name: "good-name"}}, // Hostname still uses the stricter safeText
		{{Hostname: "host", Name: "bad\"name"}, {Hostname: "good-host", Name: "good-name"}},
		{{Hostname: "host", Name: "bad\nname"}, {Hostname: "good-host", Name: "good-name"}}, // a control character (newline)
	} {
		got, err := RenderNodeConfig(fullSettings(), nodes)
		if err != nil {
			t.Fatalf("RenderNodeConfig(nodes=%+v) = %v, want no error (the unsafe entry should be skipped)", nodes, err)
		}
		cfg := string(got)
		if !strings.Contains(cfg, `regex         = "good-host"`) || !strings.Contains(cfg, `replacement   = "good-name"`) {
			t.Errorf("RenderNodeConfig(nodes=%+v) dropped the other, safe node:\n%s", nodes, cfg)
		}
		// None of the unsafe raw values leaked into the config despite being
		// skipped rather than erroring.
		for _, unsafe := range []string{`bad"host`, `bad\name`, "host with space", "bad\"name", "bad\nname"} {
			if strings.Contains(cfg, unsafe) {
				t.Errorf("RenderNodeConfig(nodes=%+v) leaked the unsafe value %q:\n%s", nodes, unsafe, cfg)
			}
		}
	}
}

// TestRenderNodeConfigNodesAllowsSpaceAndUnicode proves a node name is not
// held to safeText's stricter rule (written for URLs and logins): a space or
// non-Latin text is a normal part of a node's display name — node_labels.label
// allows both — and neither can break out of an Alloy string literal, so
// RenderNodeConfig must not skip them.
func TestRenderNodeConfigNodesAllowsSpaceAndUnicode(t *testing.T) {
	got, err := RenderNodeConfig(fullSettings(), []NodeName{
		{Hostname: "host-a", Name: "db node one"},
		{Hostname: "host-b", Name: "узел базы данных"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(got)
	for _, want := range []string{`replacement   = "db node one"`, `replacement   = "узел базы данных"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
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

// TestAlloyReplacementLiteralEscapesDollar pins the fix for a node name that
// happens to look like a regex backreference: a relabel rule's replacement is
// always expanded via regexp's ExpandString, so an un-doubled "$" is consumed
// as the start of a capture-group reference — "a$1" would otherwise render as
// plain "a" (group 1 doesn't exist and ExpandString drops it silently), not
// the literal name the operator typed.
func TestAlloyReplacementLiteralEscapesDollar(t *testing.T) {
	for name, want := range map[string]string{
		"a$1":       `"a$$1"`,
		"$$":        `"$$$$"`,
		"no-dollar": `"no-dollar"`,
	} {
		if got := alloyReplacementLiteral(name); got != want {
			t.Errorf("alloyReplacementLiteral(%q) = %s, want %s", name, got, want)
		}
	}
}

// TestRenderNodeConfigNodesDollarSurvives is the end-to-end version of
// TestAlloyReplacementLiteralEscapesDollar: a node named "a$1" must render
// with its name intact, not silently truncated to "a".
func TestRenderNodeConfigNodesDollarSurvives(t *testing.T) {
	got, err := RenderNodeConfig(fullSettings(), []NodeName{{Hostname: "host-a", Name: "a$1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `replacement   = "a$$1"`) {
		t.Errorf("node name with a literal $ not escaped:\n%s", got)
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
