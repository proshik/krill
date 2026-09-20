package appmetrics

import (
	"regexp"
	"slices"
	"testing"
)

func TestNewToken(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a) || a == b {
		t.Fatalf("token %q / %q: want 64 hex chars, distinct", a, b)
	}
}

func TestTokenHash(t *testing.T) {
	h := TokenHash("METRICS_TOKEN", "abc")
	if len(h) != 16 {
		t.Fatalf("len %d, want 16", len(h))
	}
	if h == TokenHash("KRILL_METRICS_TOKEN", "abc") {
		t.Error("hash must change with the variable name")
	}
	if h == TokenHash("METRICS_TOKEN", "abd") {
		t.Error("hash must change with the token")
	}
	if h != TokenHash("METRICS_TOKEN", "abc") {
		t.Error("hash must be deterministic")
	}
}

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"/metrics": "/metrics", "/metrics/": "/metrics", " /q/metrics ": "/q/metrics",
		"/a.b_c~d-e/f": "/a.b_c~d-e/f",
	} {
		got, ok := NormalizePath(in)
		if !ok || got != want {
			t.Errorf("NormalizePath(%q) = %q,%v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "/", "//", "metrics", "/me trics", "/m`x", `/m"x`, `/m\x`, "/m?x=1", "/m#x", "/" + string(make([]byte, 200))} {
		if _, ok := NormalizePath(bad); ok {
			t.Errorf("NormalizePath(%q) accepted", bad)
		}
	}
}

func TestValidJobAndEnvName(t *testing.T) {
	for _, j := range []string{"", "relay", "a.b:c-d_e"} {
		if !ValidJob(j) {
			t.Errorf("job %q rejected", j)
		}
	}
	for _, j := range []string{"re lay", `a"b`, "é", string(make([]byte, 65))} {
		if ValidJob(j) {
			t.Errorf("job %q accepted", j)
		}
	}
	for _, n := range []string{"METRICS_TOKEN", "_x", "a1"} {
		if !ValidEnvName(n) {
			t.Errorf("env %q rejected", n)
		}
	}
	for _, n := range []string{"", "1A", "A-B", "A B"} {
		if ValidEnvName(n) {
			t.Errorf("env %q accepted", n)
		}
	}
}

func TestValidPort(t *testing.T) {
	if !ValidPort(1) || !ValidPort(65535) || ValidPort(0) || ValidPort(65536) {
		t.Fatal("port range must be 1..65535")
	}
}

func TestHiddenPaths(t *testing.T) {
	eps := []Endpoint{{8080, "/metrics"}, {27015, "/metrics"}, {8080, "/q/metrics"}, {8080, "/metrics"}}
	got := HiddenPaths(8080, eps)
	if !slices.Equal(got, []string{"/metrics", "/q/metrics"}) {
		t.Fatalf("got %v", got)
	}
	if HiddenPaths(9999, eps) != nil {
		t.Fatal("no endpoint on the app port → nil")
	}
}
