package observability_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		"": "",
		"  https://mimir.example.com/api/v1/push ":           "https://mimir.example.com/api/v1/push",
		"http://10.0.0.5:9009/api/v1/push":                   "http://10.0.0.5:9009/api/v1/push",
		"https://logs.example.com/loki/api/v1/push?tenant=a": "https://logs.example.com/loki/api/v1/push?tenant=a",
	}
	for in, want := range ok {
		got, err := observability.NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"ftp://x/y", "mimir.example.com/api", "https://user:pw@x/y", "https://x/y#frag",
		"https://x/a b", "https://x/\"y", `https://x/\y`, "https://x/\ny", "http:///nohost",
	} {
		if _, err := observability.NormalizeURL(in); !errors.Is(err, observability.ErrInvalidURL) {
			t.Errorf("NormalizeURL(%q) err = %v, want ErrInvalidURL", in, err)
		}
	}
}

func newStore(t *testing.T) *db.Queries {
	t.Helper()
	return db.New(testutil.NewTestDB(t))
}

func TestLoadWithoutRow(t *testing.T) {
	s, err := observability.Load(context.Background(), newStore(t))
	if err != nil || s.Enabled || s.Metrics.Configured() || s.Logs.Configured() {
		t.Fatalf("Load on an empty table = %+v, %v", s, err)
	}
}

func TestSaveLoadRoundTripAndPasswordRules(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	ctx := context.Background()
	q := newStore(t)

	in := observability.Input{
		Metrics: observability.TargetInput{URL: "https://m.example.com/api/v1/push", User: "tenant", Password: "mpw"},
		Logs:    observability.TargetInput{URL: "https://l.example.com/loki/api/v1/push", User: "loki", Password: "lpw"},
	}
	if err := observability.Save(ctx, q, in); err != nil {
		t.Fatal(err)
	}
	row, _ := q.GetObservabilitySettings(ctx)
	if !strings.HasPrefix(row.MetricsPassword, "enc:") || strings.Contains(row.LogsPassword, "lpw") {
		t.Errorf("passwords not encrypted at rest: %q %q", row.MetricsPassword, row.LogsPassword)
	}
	s, err := observability.Load(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	want := observability.Target{URL: "https://m.example.com/api/v1/push", User: "tenant", Password: "mpw"}
	if s.Metrics != want || s.Logs.Password != "lpw" || s.Enabled {
		t.Errorf("loaded = %+v", s)
	}

	// An empty password keeps the stored one.
	in.Metrics.Password, in.Logs.Password = "", ""
	if err := observability.Save(ctx, q, in); err != nil {
		t.Fatal(err)
	}
	if s, _ = observability.Load(ctx, q); s.Metrics.Password != "mpw" || s.Logs.Password != "lpw" {
		t.Errorf("password not kept: %+v", s)
	}

	// No user drops basic auth entirely; no URL drops the whole target.
	in.Metrics.User = ""
	in.Logs = observability.TargetInput{User: "loki", Password: "x"}
	if err := observability.Save(ctx, q, in); err != nil {
		t.Fatal(err)
	}
	s, _ = observability.Load(ctx, q)
	if s.Metrics.User != "" || s.Metrics.Password != "" || s.Logs != (observability.Target{}) {
		t.Errorf("after clearing = %+v", s)
	}
}

func TestSaveRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	q := newStore(t)
	cases := []struct {
		in   observability.Input
		want error
	}{
		{observability.Input{Metrics: observability.TargetInput{URL: "nope"}}, observability.ErrInvalidURL},
		{observability.Input{Logs: observability.TargetInput{URL: "https://l/x", User: "a b"}}, observability.ErrInvalidUser},
		{observability.Input{Logs: observability.TargetInput{URL: "https://l/x", User: `a"b`}}, observability.ErrInvalidUser},
		{observability.Input{Logs: observability.TargetInput{URL: "https://l/x", User: "a", Password: strings.Repeat("p", 4097)}}, observability.ErrInvalidPassword},
	}
	for _, c := range cases {
		if err := observability.Save(ctx, q, c.in); !errors.Is(err, c.want) {
			t.Errorf("Save(%+v) = %v, want %v", c.in, err, c.want)
		}
	}
	if _, err := q.GetObservabilitySettings(ctx); err == nil {
		t.Error("a rejected save stored a row")
	}
}

func TestSetEnabled(t *testing.T) {
	ctx := context.Background()
	q := newStore(t)
	if err := observability.SetEnabled(ctx, q, true); !errors.Is(err, observability.ErrNotSaved) {
		t.Fatalf("enable before save = %v", err)
	}
	if err := observability.Save(ctx, q, observability.Input{}); err != nil {
		t.Fatal(err)
	}
	if err := observability.SetEnabled(ctx, q, true); !errors.Is(err, observability.ErrNothingConfigured) {
		t.Fatalf("enable with nothing configured = %v", err)
	}
	if err := observability.Save(ctx, q, observability.Input{Logs: observability.TargetInput{URL: "http://loki:3100/loki/api/v1/push"}}); err != nil {
		t.Fatal(err)
	}
	if err := observability.SetEnabled(ctx, q, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := observability.Load(ctx, q); !s.Enabled {
		t.Error("not enabled")
	}
	// Clearing every address while enabled is refused.
	if err := observability.Save(ctx, q, observability.Input{}); !errors.Is(err, observability.ErrNothingConfigured) {
		t.Errorf("clearing an enabled config = %v", err)
	}
	if err := observability.SetEnabled(ctx, q, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := observability.Load(ctx, q); s.Enabled {
		t.Error("still enabled")
	}
}

func TestLoadUndecryptable(t *testing.T) {
	ctx := context.Background()
	q := newStore(t)
	secret.Init("old-key")
	if err := observability.Save(ctx, q, observability.Input{Metrics: observability.TargetInput{URL: "https://m/x", User: "u", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	secret.Init("new-key")
	defer secret.Init("")
	if _, err := observability.Load(ctx, q); !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("Load = %v, want ErrUndecryptable", err)
	}
	if err := observability.SetEnabled(ctx, q, true); !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("SetEnabled = %v, want ErrUndecryptable", err)
	}
	// Saving with an empty password keeps the stored ciphertext untouched.
	if err := observability.Save(ctx, q, observability.Input{Metrics: observability.TargetInput{URL: "https://m/x", User: "u"}}); err != nil {
		t.Fatalf("save while undecryptable: %v", err)
	}
}
