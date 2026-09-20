package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

type fakeObsCtl struct {
	mu       sync.Mutex
	triggers int
	status   observability.Status
}

func (f *fakeObsCtl) Trigger() { f.mu.Lock(); f.triggers++; f.mu.Unlock() }
func (f *fakeObsCtl) Status(context.Context) observability.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}
func (f *fakeObsCtl) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.triggers }

type obsEnv struct {
	h       http.Handler
	q       *db.Queries
	ctl     *fakeObsCtl
	checked []observability.Settings
	report  observability.Report
	base    string
	cookie  *http.Cookie
}

// newObsEnv wires the page like main does; wired=false leaves it unwired.
// The signed-in user owns the organization and is an instance operator.
func newObsEnv(t *testing.T, wired bool) *obsEnv {
	t.Helper()
	q := db.New(testutil.NewTestDB(t))
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	env := &obsEnv{q: q, ctl: &fakeObsCtl{}}
	if wired {
		srv.SetObservability(env.ctl, func(_ context.Context, s observability.Settings) observability.Report {
			env.checked = append(env.checked, s)
			return env.report
		})
	}
	env.h = srv.Router()
	uid := mkUser(t, q, "op@k.local")
	if err := q.SetUserAdmin(context.Background(), db.SetUserAdminParams{ID: uid, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	o, err := orgSvc.CreateOrg(context.Background(), uid, "Ops")
	if err != nil {
		t.Fatal(err)
	}
	env.base = "/orgs/" + i64(o.ID) + "/observability"
	env.cookie = loginAs(t, q, "op@k.local")
	return env
}

func (e *obsEnv) do(t *testing.T, method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// flashText (decoded message of the flash cookie) is already defined in
// placement_test.go — reused here rather than redeclared.

func fullForm() url.Values {
	return url.Values{
		"metrics_url": {"https://mimir.example.com/api/v1/push"}, "metrics_user": {"tenant"}, "metrics_password": {"s3cret-m"},
		"logs_url": {"https://loki.example.com/loki/api/v1/push"}, "logs_user": {"loki"}, "logs_password": {"s3cret-l"},
	}
}

func TestObservabilityRequiresInstanceAdmin(t *testing.T) {
	env := newObsEnv(t, true)
	uid := mkUser(t, env.q, "owner@k.local")
	o, _ := org.NewService(env.q).CreateOrg(context.Background(), uid, "Plain")
	base := "/orgs/" + i64(o.ID) + "/observability"
	cookie := loginAs(t, env.q, "owner@k.local")
	if rec := env.do(t, http.MethodGet, base, nil, cookie); rec.Code != http.StatusNotFound {
		t.Errorf("GET = %d, want 404", rec.Code)
	}
	for _, p := range []string{"", "/check", "/enable", "/disable"} {
		if rec := env.do(t, http.MethodPost, base+p, fullForm(), cookie); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, rec.Code)
		}
	}
	if env.ctl.count() != 0 || len(env.checked) != 0 {
		t.Error("a refused request reached the agent")
	}
	if _, err := env.q.GetObservabilitySettings(context.Background()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a refused request stored settings (err=%v, want pgx.ErrNoRows)", err)
	}
}

func TestObservabilityUnwired(t *testing.T) {
	env := newObsEnv(t, false)
	rec := env.do(t, http.MethodGet, env.base, nil, env.cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not available") {
		t.Errorf("page = %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, env.base, fullForm(), env.cookie)
	if !hasErrFlash(rec) {
		t.Error("save accepted while unwired")
	}
}

func TestObservabilitySaveEnableDisable(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	env := newObsEnv(t, true)
	ctx := context.Background()

	if rec := env.do(t, http.MethodPost, env.base+"/enable", url.Values{}, env.cookie); !hasErrFlash(rec) {
		t.Error("enable before save accepted")
	}
	if rec := env.do(t, http.MethodPost, env.base, url.Values{"metrics_url": {"not a url"}}, env.cookie); !hasErrFlash(rec) {
		t.Error("invalid URL accepted")
	}
	rec := env.do(t, http.MethodPost, env.base, fullForm(), env.cookie)
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("save = %d %q", rec.Code, flashCookieValue(rec))
	}
	if env.ctl.count() != 0 {
		t.Error("saving while off triggered a reconcile")
	}
	s, err := observability.Load(ctx, env.q)
	if err != nil || s.Metrics.Password != "s3cret-m" || s.Logs.User != "loki" {
		t.Fatalf("stored = %+v %v", s, err)
	}

	page := env.do(t, http.MethodGet, env.base, nil, env.cookie).Body.String()
	for _, secretText := range []string{"s3cret-m", "s3cret-l"} {
		if strings.Contains(page, secretText) {
			t.Errorf("page shows a password")
		}
	}
	if !strings.Contains(page, "https://mimir.example.com/api/v1/push") || !strings.Contains(page, "leave empty to keep") {
		t.Error("page does not show the stored settings")
	}

	if rec := env.do(t, http.MethodPost, env.base+"/enable", url.Values{}, env.cookie); hasErrFlash(rec) {
		t.Fatalf("enable: %q", flashCookieValue(rec))
	}
	if s, _ := observability.Load(ctx, env.q); !s.Enabled || env.ctl.count() != 1 {
		t.Errorf("enabled=%v triggers=%d", s.Enabled, env.ctl.count())
	}

	// Saving while on applies the change; an empty password keeps the old one.
	form := fullForm()
	form.Set("metrics_password", "")
	env.do(t, http.MethodPost, env.base, form, env.cookie)
	if s, _ := observability.Load(ctx, env.q); s.Metrics.Password != "s3cret-m" || env.ctl.count() != 2 {
		t.Errorf("password=%q triggers=%d", s.Metrics.Password, env.ctl.count())
	}

	env.do(t, http.MethodPost, env.base+"/disable", url.Values{}, env.cookie)
	if s, _ := observability.Load(ctx, env.q); s.Enabled || env.ctl.count() != 3 {
		t.Errorf("after disable: enabled=%v triggers=%d", s.Enabled, env.ctl.count())
	}
}

// TestObservabilitySaveUndecryptablePassword covers the fix for the finding
// in review round 1: a save must still trigger a reconcile when the
// (raw, undecrypted) enabled bit is on, even if a stored password can no
// longer be decrypted under the current KRILL_SECRET_KEY — and the response
// must say so rather than claim plain success.
func TestObservabilitySaveUndecryptablePassword(t *testing.T) {
	secret.Init("old-key")
	defer secret.Init("")
	env := newObsEnv(t, true)

	if rec := env.do(t, http.MethodPost, env.base, fullForm(), env.cookie); hasErrFlash(rec) {
		t.Fatalf("initial save: %q", flashCookieValue(rec))
	}
	if rec := env.do(t, http.MethodPost, env.base+"/enable", url.Values{}, env.cookie); hasErrFlash(rec) {
		t.Fatalf("enable: %q", flashCookieValue(rec))
	}
	if env.ctl.count() != 1 {
		t.Fatalf("triggers after enable = %d, want 1", env.ctl.count())
	}

	// Rotate the key: the stored metrics password can no longer be decrypted.
	secret.Init("new-key")

	// Save again (empty password field keeps the now-undecryptable stored
	// value). The settings are still enabled, so this must still trigger a
	// reconcile — but the response must flash the undecryptable error, not
	// a plain success.
	form := fullForm()
	form.Set("metrics_password", "")
	rec := env.do(t, http.MethodPost, env.base, form, env.cookie)
	if !hasErrFlash(rec) {
		t.Fatalf("save with an undecryptable stored password = %q, want an error flash", flashCookieValue(rec))
	}
	if msg := flashText(rec); !strings.Contains(msg, "cannot be decrypted") {
		t.Errorf("flash = %q, want the undecryptable-password text", msg)
	}
	if env.ctl.count() != 2 {
		t.Errorf("triggers = %d, want 2 (the reconcile must still run despite the decrypt failure)", env.ctl.count())
	}
}

// TestObservabilitySaveValidationErrors covers the remaining Save error ->
// flash mappings not already exercised by TestObservabilitySaveEnableDisable
// (which covers ErrInvalidURL).
func TestObservabilitySaveValidationErrors(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	env := newObsEnv(t, true)

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "invalid user",
			form: url.Values{"metrics_url": {"https://mimir.example.com/api/v1/push"}, "metrics_user": {"bad user"}},
			want: "must not contain spaces",
		},
		{
			name: "password too long",
			form: url.Values{"metrics_url": {"https://mimir.example.com/api/v1/push"}, "metrics_user": {"tenant"}, "metrics_password": {strings.Repeat("x", 4097)}},
			want: "too long",
		},
	}
	for _, c := range cases {
		rec := env.do(t, http.MethodPost, env.base, c.form, env.cookie)
		if !hasErrFlash(rec) {
			t.Errorf("%s: accepted, want an error flash", c.name)
			continue
		}
		if msg := flashText(rec); !strings.Contains(msg, c.want) {
			t.Errorf("%s: flash = %q, want it to contain %q", c.name, msg, c.want)
		}
	}

	// An empty submit while something is configured is refused with the
	// clear-confirmation flash (Task B), not silently treated as a clear.
	if rec := env.do(t, http.MethodPost, env.base, fullForm(), env.cookie); hasErrFlash(rec) {
		t.Fatalf("save: %q", flashCookieValue(rec))
	}
	if rec := env.do(t, http.MethodPost, env.base+"/enable", url.Values{}, env.cookie); hasErrFlash(rec) {
		t.Fatalf("enable: %q", flashCookieValue(rec))
	}
	rec := env.do(t, http.MethodPost, env.base, url.Values{}, env.cookie)
	if !hasErrFlash(rec) {
		t.Fatal("an empty submit over a configured target was accepted")
	}
	if msg := flashText(rec); !strings.Contains(msg, `tick "Remove these settings"`) {
		t.Errorf("flash = %q", msg)
	}

	// ErrNothingConfigured: confirming the clear on both targets while
	// enabled is still refused — clearing everything would leave the agent
	// enabled with nothing to send.
	rec = env.do(t, http.MethodPost, env.base, url.Values{"metrics_clear": {"on"}, "logs_clear": {"on"}}, env.cookie)
	if !hasErrFlash(rec) {
		t.Fatal("clearing both addresses while enabled was accepted")
	}
	if msg := flashText(rec); !strings.Contains(msg, "Set a metrics or a logs address") {
		t.Errorf("flash = %q", msg)
	}
}

// TestObservabilitySaveClearRequiresConfirmation is the server-level half of
// Task B: an accidentally empty URL on an already-configured target is
// refused, and ticking the confirmation checkbox is what actually clears it.
func TestObservabilitySaveClearRequiresConfirmation(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	env := newObsEnv(t, true)

	if rec := env.do(t, http.MethodPost, env.base, fullForm(), env.cookie); hasErrFlash(rec) {
		t.Fatalf("save: %q", flashCookieValue(rec))
	}

	// Submitting the logs target with a blank URL and no confirmation is
	// refused, and the stored logs target is untouched.
	form := fullForm()
	form.Set("logs_url", "")
	rec := env.do(t, http.MethodPost, env.base, form, env.cookie)
	if !hasErrFlash(rec) {
		t.Fatal("a blank URL without confirmation was accepted")
	}
	if msg := flashText(rec); !strings.Contains(msg, `tick "Remove these settings"`) {
		t.Errorf("flash = %q", msg)
	}
	s, err := observability.Load(context.Background(), env.q)
	if err != nil || !s.Logs.Configured() {
		t.Errorf("a refused clear erased the logs target: %+v, %v", s, err)
	}

	// Ticking the checkbox confirms the clear.
	form.Set("logs_clear", "on")
	if rec := env.do(t, http.MethodPost, env.base, form, env.cookie); hasErrFlash(rec) {
		t.Fatalf("confirmed clear: %q", flashCookieValue(rec))
	}
	if s, _ := observability.Load(context.Background(), env.q); s.Logs.Configured() {
		t.Errorf("the confirmed clear did not remove the logs target: %+v", s)
	}
}

// TestObservabilitySaveClearWithAddressRejected is M2: ticking "Remove these
// settings" while the address field still holds a value is refused, not
// silently ignored in favor of saving the address as if the box were
// unchecked.
func TestObservabilitySaveClearWithAddressRejected(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	env := newObsEnv(t, true)

	if rec := env.do(t, http.MethodPost, env.base, fullForm(), env.cookie); hasErrFlash(rec) {
		t.Fatalf("save: %q", flashCookieValue(rec))
	}

	form := fullForm()
	form.Set("metrics_clear", "on") // metrics_url is still set from fullForm()
	rec := env.do(t, http.MethodPost, env.base, form, env.cookie)
	if !hasErrFlash(rec) {
		t.Fatal("Clear ticked with a non-empty address was accepted")
	}
	if msg := flashText(rec); !strings.Contains(msg, "clear it") {
		t.Errorf("flash = %q", msg)
	}
	s, err := observability.Load(context.Background(), env.q)
	if err != nil || s.Metrics.URL != "https://mimir.example.com/api/v1/push" || s.Metrics.Password != "s3cret-m" {
		t.Errorf("a refused Clear-with-address changed the row: %+v, %v", s, err)
	}
}

// A kept password must not follow the address to another server.
func TestObservabilitySavePasswordNeedsSameServer(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	env := newObsEnv(t, true)
	if rec := env.do(t, http.MethodPost, env.base, fullForm(), env.cookie); hasErrFlash(rec) {
		t.Fatalf("save: %q", flashCookieValue(rec))
	}
	form := fullForm()
	form.Set("metrics_url", "https://evil.example.net/api/v1/push")
	form.Set("metrics_password", "")
	rec := env.do(t, http.MethodPost, env.base, form, env.cookie)
	if !hasErrFlash(rec) {
		t.Fatal("a host change without a password was accepted")
	}
	if msg := flashText(rec); !strings.Contains(msg, "points to a different server") {
		t.Errorf("flash = %q", msg)
	}
	s, err := observability.Load(context.Background(), env.q)
	if err != nil || s.Metrics.URL != "https://mimir.example.com/api/v1/push" {
		t.Errorf("stored = %+v %v", s, err)
	}
}

func TestObservabilityCheck(t *testing.T) {
	env := newObsEnv(t, true)
	if rec := env.do(t, http.MethodPost, env.base+"/check", url.Values{}, env.cookie); !hasErrFlash(rec) || len(env.checked) != 0 {
		t.Error("check without settings was run")
	}
	env.do(t, http.MethodPost, env.base, fullForm(), env.cookie)

	env.report = observability.Report{
		Metrics: &observability.Result{OK: true, Key: "obs.check.ok_v1_only"},
		Logs:    &observability.Result{Key: "obs.check.auth", Detail: "401"},
	}
	// A failed check lands back on the page regardless of the Referer.
	rec := env.do(t, http.MethodPost, env.base+"/check", url.Values{}, env.cookie)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != env.base {
		t.Errorf("failed check redirect = %d %q, want 303 %q", rec.Code, loc, env.base)
	}
	if len(env.checked) != 1 || env.checked[0].Logs.Password != "s3cret-l" {
		t.Fatalf("checked = %+v", env.checked)
	}
	if !hasErrFlash(rec) {
		t.Error("a failed logs check flashed success")
	}
	msg := flashText(rec)
	if !strings.Contains(msg, "Metrics: the address and credentials are fine") || !strings.Contains(msg, "Logs: wrong user or password (HTTP 401)") {
		t.Errorf("flash = %q", msg)
	}

	env.report = observability.Report{Metrics: &observability.Result{OK: true, Key: "obs.check.ok"}}
	rec = env.do(t, http.MethodPost, env.base+"/check", url.Values{}, env.cookie)
	if hasErrFlash(rec) || !strings.Contains(flashText(rec), "Logs: not configured") {
		t.Errorf("flash = %q", flashCookieValue(rec))
	}
}

func TestObservabilityStatusOnPage(t *testing.T) {
	env := newObsEnv(t, true)
	env.ctl.status = observability.Status{
		Busy:    true,
		LastErr: "the agent is not running on every node",
		Service: docker.ServiceState{Found: true, Running: 1, Desired: 2, Failed: 1},
	}
	page := env.do(t, http.MethodGet, env.base, nil, env.cookie).Body.String()
	for _, want := range []string{"Agents running: 1 of 2", "Failed tasks: 1", "Applying the settings", "not running on every node", "256 MiB"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// TestObservabilityStatusShowsPartialCoverage is the page-rendering half of
// Task C: a node still on the previous settings must be named, and the text
// must say so — not merely that the node is unreachable — because that is
// the whole point after rotating a push credential.
func TestObservabilityStatusShowsPartialCoverage(t *testing.T) {
	env := newObsEnv(t, true)
	env.ctl.status = observability.Status{
		Service:  docker.ServiceState{Found: true, Running: 1, Desired: 1},
		Coverage: observability.Coverage{Expected: 2, Deployed: 1, Missing: []string{"worker-1"}},
	}
	page := env.do(t, http.MethodGet, env.base, nil, env.cookie).Body.String()
	for _, want := range []string{"Deployed on 1 of 2 nodes", "worker-1", "previous settings", "next reconcile pass"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// TestObservabilityStatusHidesCoverageWhenFull proves the note only appears
// when something is actually missing — full coverage adds no extra line.
func TestObservabilityStatusHidesCoverageWhenFull(t *testing.T) {
	env := newObsEnv(t, true)
	env.ctl.status = observability.Status{
		Service:  docker.ServiceState{Found: true, Running: 2, Desired: 2},
		Coverage: observability.Coverage{Expected: 2, Deployed: 2},
	}
	page := env.do(t, http.MethodGet, env.base, nil, env.cookie).Body.String()
	if strings.Contains(page, "previous settings") {
		t.Error("page shows a coverage note with nothing missing")
	}
}

func (f *fakeObsCtl) TriggerNetworks() { f.Trigger() }
