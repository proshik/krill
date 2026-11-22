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
	"time"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/statebackup"
	"github.com/proshik/krill/internal/testutil"
)

const sbkSecret = "s3cr3t-s3-key-DO-NOT-SHOW"

type fakeSbk struct {
	mu       sync.Mutex
	view     statebackup.Settings
	viewErr  error
	saveErr  error
	saved    []statebackup.Input
	enabled  []bool
	startErr error
	starts   []statebackup.Trigger
	inFlight int
	runs     []statebackup.Run
	last     statebackup.Run
	hasLast  bool
}

func (f *fakeSbk) View(context.Context) (statebackup.Settings, error) { return f.view, f.viewErr }
func (f *fakeSbk) Save(_ context.Context, in statebackup.Input) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, in)
	return nil
}
func (f *fakeSbk) SetEnabled(_ context.Context, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = append(f.enabled, on)
	return nil
}
func (f *fakeSbk) Start(t statebackup.Trigger) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.starts = append(f.starts, t)
	return nil
}
func (f *fakeSbk) InFlight() int { return f.inFlight }
func (f *fakeSbk) Runs(context.Context, int) ([]statebackup.Run, error) {
	return f.runs, nil
}
func (f *fakeSbk) LastSuccess(context.Context) (statebackup.Run, bool, error) {
	return f.last, f.hasLast, nil
}

type sbkEnv struct {
	h      http.Handler
	q      *db.Queries
	ctl    *fakeSbk
	base   string
	cookie *http.Cookie
}

// newSbkEnv: the signed-in user owns the organization and is an instance
// operator. wired=false leaves the page unwired.
func newSbkEnv(t *testing.T, wired, secretKeySet bool) *sbkEnv {
	t.Helper()
	q := db.New(testutil.NewTestDB(t))
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	env := &sbkEnv{q: q, ctl: &fakeSbk{}}
	if wired {
		srv.SetStateBackup(env.ctl, secretKeySet)
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
	env.base = "/orgs/" + i64(o.ID) + "/krill-backups"
	env.cookie = loginAs(t, q, "op@k.local")
	return env
}

func (e *sbkEnv) do(t *testing.T, method, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
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

func (e *sbkEnv) page(t *testing.T) string {
	t.Helper()
	rec := e.do(t, http.MethodGet, e.base, nil, e.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	return rec.Body.String()
}

func sbkForm() url.Values {
	return url.Values{
		"endpoint": {"https://s3.example.com"}, "bucket": {"b"}, "region": {"r"}, "access_key": {"ak"},
		"secret_key": {sbkSecret}, "prefix": {"p"}, "recipient": {"age1xyz"},
		"schedule_preset": {"daily"}, "retention": {"14"},
	}
}

func readySettings() statebackup.Settings {
	return statebackup.Settings{
		Enabled: true, Endpoint: "https://s3.example.com", Bucket: "b", Region: "r", AccessKey: "ak",
		HasSecretKey: true, Prefix: "p", Recipient: "age1abc", Schedule: "0 3 * * *", Retention: 14,
	}
}

func TestStateBackupPageInstanceAdminOnly(t *testing.T) {
	env := newSbkEnv(t, true, true)
	uid := mkUser(t, env.q, "owner@k.local")
	o, _ := org.NewService(env.q).CreateOrg(context.Background(), uid, "Plain")
	base := "/orgs/" + i64(o.ID) + "/krill-backups"
	cookie := loginAs(t, env.q, "owner@k.local")
	if rec := env.do(t, http.MethodGet, base, nil, cookie); rec.Code != http.StatusNotFound {
		t.Errorf("GET = %d, want 404", rec.Code)
	}
	for _, p := range []string{"", "/run", "/enabled"} {
		if rec := env.do(t, http.MethodPost, base+p, sbkForm(), cookie); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, rec.Code)
		}
	}
	if len(env.ctl.saved)+len(env.ctl.starts)+len(env.ctl.enabled) != 0 {
		t.Error("a refused request reached the service")
	}
	if rec := env.do(t, http.MethodGet, env.base, nil, env.cookie); rec.Code != http.StatusOK {
		t.Errorf("instance admin GET = %d", rec.Code)
	}
}

func TestStateBackupPageNeverRendersSecret(t *testing.T) {
	env := newSbkEnv(t, true, true)
	env.ctl.view = readySettings()
	env.ctl.view.SecretKey = sbkSecret // even if a service leaked it, the page must not print it
	body := env.page(t)
	if strings.Contains(body, sbkSecret) {
		t.Fatal("page renders the secret key")
	}
	i := strings.Index(body, `name="secret_key"`)
	if i < 0 {
		t.Fatal("no secret key input")
	}
	tag := body[strings.LastIndex(body[:i], "<input"):]
	tag = tag[:strings.Index(tag, ">")]
	if strings.Contains(tag, "value=") {
		t.Errorf("secret input has a value: %s", tag)
	}
	if !strings.Contains(body, "stored") {
		t.Error("no stored hint")
	}
}

func TestStateBackupPageStates(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		env := newSbkEnv(t, true, true)
		if b := env.page(t); !strings.Contains(b, "not set up") || !strings.Contains(b, "never") {
			t.Errorf("unexpected page")
		}
	})
	t.Run("active shows next run", func(t *testing.T) {
		env := newSbkEnv(t, true, true)
		env.ctl.view = readySettings()
		next, _ := statebackup.NextRun("0 3 * * *", time.Now())
		b := env.page(t)
		if !strings.Contains(b, "active") || !strings.Contains(b, next.Format("2006-01-02 15:04")) {
			t.Errorf("active/next run missing (want %s)", next.Format("2006-01-02 15:04"))
		}
	})
	t.Run("on demand", func(t *testing.T) {
		env := newSbkEnv(t, true, true)
		env.ctl.view = readySettings()
		env.ctl.view.Schedule = ""
		if b := env.page(t); !strings.Contains(b, "on demand only") {
			t.Error("on demand text missing")
		}
	})
	t.Run("paused", func(t *testing.T) {
		env := newSbkEnv(t, true, true)
		env.ctl.view = readySettings()
		env.ctl.view.Enabled = false
		b := env.page(t)
		// Paused: the switch is off and turning it on posts enabled=1.
		if form := formHTML(b, env.base+"/enabled"); !strings.Contains(b, "paused") || !strings.Contains(form, `aria-checked="false"`) || !strings.Contains(form, `name="enabled" value="1"`) {
			t.Error("paused/resume missing")
		}
		if strings.Contains(formHTML(b, env.base+"/run"), "disabled") {
			t.Error("back up now is disabled while paused")
		}
	})
	t.Run("running", func(t *testing.T) {
		env := newSbkEnv(t, true, true)
		env.ctl.view = readySettings()
		env.ctl.inFlight = 1
		b := env.page(t)
		if !strings.Contains(b, "running") || !strings.Contains(formHTML(b, env.base+"/run"), "disabled") {
			t.Error("running indicator or disabled button missing")
		}
	})
	t.Run("runs and last success", func(t *testing.T) {
		env := newSbkEnv(t, true, true)
		env.ctl.view = readySettings()
		at := time.Date(2026, 11, 19, 3, 0, 5, 0, time.UTC)
		env.ctl.hasLast = true
		env.ctl.last = statebackup.Run{Status: "ok", SizeBytes: 3 << 20, FinishedAt: at, StartedAt: at}
		env.ctl.runs = []statebackup.Run{
			{Status: "error", Trigger: statebackup.TriggerManual, StartedAt: at, Error: "dump failed: boom"},
			{Status: "ok", Trigger: statebackup.TriggerSchedule, StartedAt: at, SizeBytes: 3 << 20, ObjectKey: "p/krill-state/x.sql.gz.age"},
		}
		b := env.page(t)
		for _, want := range []string{"dump failed: boom", "p/krill-state/x.sql.gz.age", "3.0 MiB", "2026-11-19"} {
			if !strings.Contains(b, want) {
				t.Errorf("page lacks %q", want)
			}
		}
	})
}

func TestStateBackupWarnings(t *testing.T) {
	env := newSbkEnv(t, true, true)
	b := env.page(t)
	if !strings.Contains(b, "does not keep the age private key") {
		t.Error("permanent warning missing")
	}
	if strings.Contains(b, "stored secrets are in plain text") {
		t.Error("plaintext note shown although KRILL_SECRET_KEY is set")
	}
	env = newSbkEnv(t, true, false)
	if b := env.page(t); !strings.Contains(b, "stored secrets are in plain text") {
		t.Error("plaintext note missing")
	}
}

func TestSaveStateBackupErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{statebackup.ErrInvalidRecipient, "public key"},
		{statebackup.ErrSecretKeyRequired, "secret key again"},
		{&statebackup.StorageError{Msg: "boom: NoSuchBucket"}, "boom: NoSuchBucket"},
		{statebackup.ErrRetention, "keep"},
		{statebackup.ErrInvalidSchedule, "schedule"},
		{statebackup.ErrBucketRequired, "bucket"},
	}
	for _, c := range cases {
		env := newSbkEnv(t, true, true)
		env.ctl.saveErr = c.err
		rec := env.do(t, http.MethodPost, env.base, sbkForm(), env.cookie)
		if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) || !strings.Contains(flashText(rec), c.want) {
			t.Errorf("%v: code=%d flash=%q, want error flash containing %q", c.err, rec.Code, flashText(rec), c.want)
		}
		if len(env.ctl.saved) != 0 {
			t.Errorf("%v: saved", c.err)
		}
	}
	// An unexpected error is internal and shows no text.
	env := newSbkEnv(t, true, true)
	env.ctl.saveErr = errors.New("dsn=postgres://u:pw@h/db broke")
	rec := env.do(t, http.MethodPost, env.base, sbkForm(), env.cookie)
	if !hasErrFlash(rec) || strings.Contains(flashText(rec), "pw@h") {
		t.Errorf("flash = %q", flashText(rec))
	}
}

func TestSaveStateBackupPassesForm(t *testing.T) {
	env := newSbkEnv(t, true, true)
	rec := env.do(t, http.MethodPost, env.base, sbkForm(), env.cookie)
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) || len(env.ctl.saved) != 1 {
		t.Fatalf("save = %d %q saved=%d", rec.Code, flashText(rec), len(env.ctl.saved))
	}
	in := env.ctl.saved[0]
	if in.Bucket != "b" || in.SecretKey != sbkSecret || in.Preset != "daily" || in.Retention != 14 || in.Recipient != "age1xyz" {
		t.Errorf("input = %+v", in)
	}
}

func TestRunStateBackupNow(t *testing.T) {
	env := newSbkEnv(t, true, true)
	rec := env.do(t, http.MethodPost, env.base+"/run", url.Values{}, env.cookie)
	if hasErrFlash(rec) || len(env.ctl.starts) != 1 || env.ctl.starts[0] != statebackup.TriggerManual {
		t.Fatalf("run: flash=%q starts=%v", flashCookieValue(rec), env.ctl.starts)
	}
	if !strings.HasPrefix(flashCookieValue(rec), "ok:") || !strings.Contains(flashText(rec), "started") {
		t.Errorf("flash = %q", flashCookieValue(rec))
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{statebackup.ErrStateBackupRunning, "already running"},
		{statebackup.ErrNotConfigured, "not set up"},
		{statebackup.ErrUpdateInProgress, "update is in progress"},
	} {
		env.ctl.startErr = c.err
		rec := env.do(t, http.MethodPost, env.base+"/run", url.Values{}, env.cookie)
		if !hasErrFlash(rec) || !strings.Contains(flashText(rec), c.want) {
			t.Errorf("%v: flash = %q, want %q", c.err, flashText(rec), c.want)
		}
	}
}

func TestSetStateBackupEnabledDesiredState(t *testing.T) {
	env := newSbkEnv(t, true, true)
	for i := 0; i < 2; i++ {
		if rec := env.do(t, http.MethodPost, env.base+"/enabled", url.Values{"enabled": {"1"}}, env.cookie); hasErrFlash(rec) {
			t.Fatalf("enable: %q", flashText(rec))
		}
	}
	env.do(t, http.MethodPost, env.base+"/enabled", url.Values{"enabled": {"0"}}, env.cookie)
	got := env.ctl.enabled
	if len(got) != 3 || !got[0] || !got[1] || got[2] {
		t.Errorf("SetEnabled calls = %v, want [true true false]", got)
	}
}

func TestStateBackupPageRendersWithUndecryptableKey(t *testing.T) {
	// View never decrypts, so a rotated KRILL_SECRET_KEY still renders.
	env := newSbkEnv(t, true, true)
	env.ctl.view = readySettings()
	if rec := env.do(t, http.MethodGet, env.base, nil, env.cookie); rec.Code != http.StatusOK {
		t.Errorf("GET = %d", rec.Code)
	}
}

func TestStateBackupUnwired(t *testing.T) {
	env := newSbkEnv(t, false, true)
	rec := env.do(t, http.MethodGet, env.base, nil, env.cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not available") {
		t.Errorf("page = %d", rec.Code)
	}
	for _, p := range []string{"", "/run", "/enabled"} {
		if rec := env.do(t, http.MethodPost, env.base+p, sbkForm(), env.cookie); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, rec.Code)
		}
	}
}

// TestStateBackupModeOnUpdatesPage: the Updates page's pre-update line follows
// the state backup's real state.
func TestStateBackupModeOnUpdatesPage(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fakeSbk)
		want string
	}{
		{"on", func(f *fakeSbk) { f.view = readySettings() }, "will be backed up first"},
		{"paused", func(f *fakeSbk) { f.view = readySettings(); f.view.Enabled = false }, "Krill backups are paused"},
		{"off", func(f *fakeSbk) {}, "is not backed up before updates"},
	}
	for _, c := range cases {
		f := newUpdateFixture(t)
		srv := f.newServer()
		ctl := &fakeSbk{}
		c.set(ctl)
		srv.SetStateBackup(ctl, true)
		ck, ins := newUpdateFakes()
		srv.SetSelfUpdate(ck, ins)
		h := srv.Router()
		uid := mkUser(t, f.q, "op@k.local")
		if err := f.q.SetUserAdmin(context.Background(), db.SetUserAdminParams{ID: uid, IsAdmin: true}); err != nil {
			t.Fatal(err)
		}
		o, _ := f.orgSvc.CreateOrg(context.Background(), uid, "Ops")
		body := getPage(t, h, "/orgs/"+i64(o.ID)+"/updates", loginAs(t, f.q, "op@k.local"), nil).Body.String()
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: updates page lacks %q", c.name, c.want)
		}
	}
}
