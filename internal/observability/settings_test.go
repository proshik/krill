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

	// No user drops basic auth entirely; a blank URL alone (Clear not set)
	// refuses rather than dropping the whole target (Task B).
	in.Metrics.User = ""
	in.Logs = observability.TargetInput{User: "loki", Password: "x"}
	if err := observability.Save(ctx, q, in); !errors.Is(err, observability.ErrClearNotConfirmed) {
		t.Fatalf("blank URL without Clear = %v, want ErrClearNotConfirmed", err)
	}
	if s, _ = observability.Load(ctx, q); s.Logs.URL == "" {
		t.Error("a refused clear erased the logs target")
	}
	if s.Metrics.User != "tenant" {
		t.Error("a refused save on the logs target also touched metrics")
	}

	// With Clear set, the whole target is dropped as before.
	in.Logs.Clear = true
	if err := observability.Save(ctx, q, in); err != nil {
		t.Fatal(err)
	}
	s, _ = observability.Load(ctx, q)
	if s.Metrics.User != "" || s.Metrics.Password != "" || s.Logs != (observability.Target{}) {
		t.Errorf("after clearing = %+v", s)
	}
}

// TestSaveClearRequiresConfirmation is Task B: an accidentally empty URL on
// an already-configured target must not silently wipe it.
func TestSaveClearRequiresConfirmation(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	ctx := context.Background()
	q := newStore(t)

	base := observability.Input{
		Metrics: observability.TargetInput{URL: "https://m.example.com/api/v1/push", User: "tenant", Password: "mpw"},
	}
	if err := observability.Save(ctx, q, base); err != nil {
		t.Fatal(err)
	}

	// An empty URL without Clear is refused, and nothing changes.
	if err := observability.Save(ctx, q, observability.Input{Metrics: observability.TargetInput{}}); !errors.Is(err, observability.ErrClearNotConfirmed) {
		t.Fatalf("Save with a blank URL = %v, want ErrClearNotConfirmed", err)
	}
	s, err := observability.Load(ctx, q)
	if err != nil || s.Metrics.URL != base.Metrics.URL || s.Metrics.Password != "mpw" {
		t.Errorf("a refused clear changed the row: %+v, %v", s, err)
	}

	// The same empty URL with Clear set wipes the target.
	if err := observability.Save(ctx, q, observability.Input{Metrics: observability.TargetInput{Clear: true}}); err != nil {
		t.Fatal(err)
	}
	if s, _ = observability.Load(ctx, q); s.Metrics != (observability.Target{}) {
		t.Errorf("Clear did not wipe the target: %+v", s)
	}

	// Clear on a target that was never configured is a no-op, not an error:
	// there is nothing to confirm clearing.
	if err := observability.Save(ctx, q, observability.Input{Metrics: observability.TargetInput{Clear: true}}); err != nil {
		t.Errorf("Clear on an already-empty target = %v, want nil", err)
	}

	// Ticking Clear while also submitting a non-empty address is refused
	// (M2): the checkbox must not be silently ignored in favor of saving
	// whatever address happens to be in the field.
	if err := observability.Save(ctx, q, base); err != nil {
		t.Fatal(err)
	}
	in := observability.Input{Metrics: observability.TargetInput{URL: base.Metrics.URL, User: "tenant", Clear: true}}
	if err := observability.Save(ctx, q, in); !errors.Is(err, observability.ErrClearWithAddress) {
		t.Fatalf("Save with Clear and a non-empty URL = %v, want ErrClearWithAddress", err)
	}
	if s, _ = observability.Load(ctx, q); s.Metrics.URL != base.Metrics.URL || s.Metrics.Password != "mpw" {
		t.Errorf("a refused Clear-with-address changed the row: %+v", s)
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
	// An empty submit is refused without a hint of what it would have done —
	// the configured Logs target needs Clear before it gives up ErrNothingConfigured
	// as the reason (Task B: this is the accidental-blank-form case).
	if err := observability.Save(ctx, q, observability.Input{}); !errors.Is(err, observability.ErrClearNotConfirmed) {
		t.Errorf("blank submit while enabled = %v, want ErrClearNotConfirmed", err)
	}
	// Confirming the clear reaches the real guard: clearing every address
	// while enabled is refused.
	if err := observability.Save(ctx, q, observability.Input{Logs: observability.TargetInput{Clear: true}}); !errors.Is(err, observability.ErrNothingConfigured) {
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

func TestLoadForReconcile(t *testing.T) {
	ctx := context.Background()
	q := newStore(t)
	if s, err := observability.LoadForReconcile(ctx, q); err != nil || s != (observability.Settings{}) {
		t.Fatalf("no row = %+v, %v", s, err)
	}
	secret.Init("old-key")
	defer secret.Init("")
	if err := observability.Save(ctx, q, observability.Input{Metrics: observability.TargetInput{URL: "https://m/x", User: "u", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	if err := observability.SetEnabled(ctx, q, true); err != nil {
		t.Fatal(err)
	}
	secret.Init("new-key")
	// Enabled: the agent is built from the passwords, so the failure surfaces.
	if _, err := observability.LoadForReconcile(ctx, q); !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("enabled + undecryptable = %v, want ErrUndecryptable", err)
	}
	// Disabled: nothing is decrypted and the result tears the agent down.
	if err := observability.SetEnabled(ctx, q, false); err != nil {
		t.Fatal(err)
	}
	if s, err := observability.LoadForReconcile(ctx, q); err != nil || s != (observability.Settings{}) {
		t.Errorf("disabled + undecryptable = %+v, %v; want zero Settings, nil", s, err)
	}
}

func TestSavePasswordFollowsOnlyTheSameServer(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	ctx := context.Background()
	q := newStore(t)
	base := observability.Input{
		Metrics: observability.TargetInput{URL: "https://m.example.com/api/v1/push", User: "tenant", Password: "mpw"},
		Logs:    observability.TargetInput{URL: "http://l.example.com/loki/api/v1/push", User: "loki", Password: "lpw"},
	}
	if err := observability.Save(ctx, q, base); err != nil {
		t.Fatal(err)
	}
	keep := func(m, l string) observability.Input {
		return observability.Input{
			Metrics: observability.TargetInput{URL: m, User: "tenant"},
			Logs:    observability.TargetInput{URL: l, User: "loki"},
		}
	}
	for _, in := range []observability.Input{
		keep("https://other.example.com/api/v1/push", base.Logs.URL),         // host
		keep("https://m.example.com:8443/api/v1/push", base.Logs.URL),        // port
		keep("http://m.example.com/api/v1/push", base.Logs.URL),              // scheme
		keep(base.Metrics.URL, "http://l.example.com:3100/loki/api/v1/push"), // logs port
	} {
		if err := observability.Save(ctx, q, in); !errors.Is(err, observability.ErrPasswordRequired) {
			t.Errorf("Save(%+v) = %v, want ErrPasswordRequired", in, err)
		}
	}
	if s, _ := observability.Load(ctx, q); s.Metrics != (observability.Target{URL: base.Metrics.URL, User: "tenant", Password: "mpw"}) {
		t.Errorf("a refused save changed the row: %+v", s)
	}

	// The effective port counts: an explicit default port is the same server,
	// and so is a different path.
	if err := observability.Save(ctx, q, keep("https://m.example.com:443/prom/push", "http://l.example.com:80/loki/api/v1/push")); err != nil {
		t.Fatalf("same server, other path: %v", err)
	}
	s, _ := observability.Load(ctx, q)
	if s.Metrics.Password != "mpw" || s.Logs.Password != "lpw" || s.Metrics.URL != "https://m.example.com:443/prom/push" {
		t.Errorf("password not kept on a path change: %+v", s)
	}

	// A new password may go anywhere.
	in := keep("https://other.example.com/api/v1/push", s.Logs.URL)
	in.Metrics.Password = "newpw"
	if err := observability.Save(ctx, q, in); err != nil {
		t.Fatalf("host change with a new password: %v", err)
	}
	if s, _ = observability.Load(ctx, q); s.Metrics.Password != "newpw" || s.Metrics.URL != "https://other.example.com/api/v1/push" {
		t.Errorf("after host change with a new password: %+v", s)
	}
}
