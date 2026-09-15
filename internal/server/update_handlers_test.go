package server_test

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// fakeUpdateChecker stands in for *selfupdate.Checker. Every method counts
// towards calls, so a test can prove a refused request never reached it.
type fakeUpdateChecker struct {
	mu         sync.Mutex
	calls      int
	current    string
	state      selfupdate.CheckState
	newer      bool // what Available reports for state.Latest
	checkErr   error
	checkCalls int
}

func (f *fakeUpdateChecker) Current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.current
}

func (f *fakeUpdateChecker) State() selfupdate.CheckState {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.state
}

func (f *fakeUpdateChecker) Available() (selfupdate.Release, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.state.Latest, f.newer
}

func (f *fakeUpdateChecker) CheckNow(context.Context) (selfupdate.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.checkCalls++
	return f.state.Latest, f.checkErr
}

func (f *fakeUpdateChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeUpdateInstaller stands in for *selfupdate.Updater.
type fakeUpdateInstaller struct {
	mu             sync.Mutex
	calls          int
	supported      bool
	reason         string
	supportedCalls int
	busy           string
	busyCalls      int
	startErr       error
	startCalls     int
	startTag       string
	rollbackErr    error
	rollbackCalls  int
	status         selfupdate.Status
	last           *selfupdate.Result
	previous       string
}

func (f *fakeUpdateInstaller) Supported() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.supportedCalls++
	return f.supported, f.reason
}

func (f *fakeUpdateInstaller) Busy(context.Context) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.busyCalls++
	return f.busy
}

func (f *fakeUpdateInstaller) Start(tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.startCalls++
	f.startTag = tag
	return f.startErr
}

func (f *fakeUpdateInstaller) Rollback() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.rollbackCalls++
	return f.rollbackErr
}

func (f *fakeUpdateInstaller) Status() selfupdate.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.status
}

func (f *fakeUpdateInstaller) LastResult() (selfupdate.Result, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.last == nil {
		return selfupdate.Result{}, false
	}
	return *f.last, true
}

func (f *fakeUpdateInstaller) Previous(context.Context) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.previous, f.previous != ""
}

func (f *fakeUpdateInstaller) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const (
	updCurrent = "v0.1.0"
	updLatest  = "v0.2.0"
)

// newUpdateFakes returns fakes for an instance that runs updCurrent, knows a
// newer updLatest, can update itself and has nothing running.
func newUpdateFakes() (*fakeUpdateChecker, *fakeUpdateInstaller) {
	c := &fakeUpdateChecker{
		current: updCurrent,
		state: selfupdate.CheckState{
			Latest:    selfupdate.Release{Tag: updLatest, URL: "https://github.com/proshik/krill/releases/tag/" + updLatest},
			CheckedAt: time.Date(2026, 9, 15, 8, 30, 0, 0, time.UTC),
		},
		newer: true,
	}
	i := &fakeUpdateInstaller{supported: true, status: selfupdate.Status{Phase: selfupdate.PhaseIdle}}
	return c, i
}

type updateFixture struct {
	q      *db.Queries
	orgSvc *org.Service
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	q := db.New(testutil.NewTestDB(t))
	return &updateFixture{q: q, orgSvc: org.NewService(q)}
}

// newServer builds a fresh server over the fixture's database.
func (f *updateFixture) newServer() *server.Server {
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	return server.New(cfg, auth.NewService(f.q), f.orgSvc, f.q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(f.q), hub, "krill-net"))
}

// handler is newServer wired to the fakes. With nil fakes SetSelfUpdate is
// never called, i.e. the feature is not wired.
func (f *updateFixture) handler(c *fakeUpdateChecker, i *fakeUpdateInstaller) http.Handler {
	srv := f.newServer()
	if c != nil && i != nil {
		srv.SetSelfUpdate(c, i)
	}
	return srv.Router()
}

// plainOwner creates an organization owned by a user who is NOT an instance
// operator.
func (f *updateFixture) plainOwner(t *testing.T, email string) (base string, cookie *http.Cookie) {
	t.Helper()
	uid := mkUser(t, f.q, email)
	o, err := f.orgSvc.CreateOrg(context.Background(), uid, "Org "+email)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return "/orgs/" + i64(o.ID), loginAs(t, f.q, email)
}

func getPage(t *testing.T, h http.Handler, target string, cookie *http.Cookie, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(cookie)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// formHTML returns the <form> element whose action is action, or "".
func formHTML(body, action string) string {
	i := strings.Index(body, `action="`+action+`"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(body[:i], "<form")
	end := strings.Index(body[i:], "</form>")
	if start < 0 || end < 0 {
		return ""
	}
	return body[start : i+end]
}

// TestUpdatesRequireInstanceAdmin: an organization owner who is not an
// instance operator gets 404 on every update route, and none of them reaches
// the checker or the installer.
func TestUpdatesRequireInstanceAdmin(t *testing.T) {
	f := newUpdateFixture(t)
	c, i := newUpdateFakes()
	h := f.handler(c, i)
	base, cookie := f.plainOwner(t, "upd-owner@k.local")

	for _, target := range []string{base + "/updates", base + "/updates/status?from=" + updCurrent} {
		if rec := getPage(t, h, target, cookie, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s as a plain owner: want 404, got %d", target, rec.Code)
		}
	}
	for _, target := range []string{base + "/updates/check", base + "/updates/install", base + "/updates/rollback"} {
		if rec := postForm(t, h, target, cookie, url.Values{"tag": {updLatest}}); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s as a plain owner: want 404, got %d", target, rec.Code)
		}
	}
	if n := c.callCount() + i.callCount(); n != 0 {
		t.Errorf("a refused request reached the fakes %d times, want 0", n)
	}
}

func TestUpdatesPage(t *testing.T) {
	f := newUpdateFixture(t)
	base, cookie, _ := nodesOrg(t, f.q, f.orgSvc, "upd-admin@k.local", "OrgUpd")
	install := base + "/updates/install"
	rollback := base + "/updates/rollback"

	t.Run("update available", func(t *testing.T) {
		c, i := newUpdateFakes()
		rec := getPage(t, f.handler(c, i), base+"/updates", cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /updates: want 200, got %d", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{updCurrent, updLatest, "Release notes", "Last checked 2026-09-15 08:30 UTC"} {
			if !strings.Contains(body, want) {
				t.Errorf("page does not contain %q", want)
			}
		}
		form := formHTML(body, install)
		if form == "" {
			t.Fatalf("no install form in the page")
		}
		if !strings.Contains(form, `name="tag"`) || !strings.Contains(form, `value="`+updLatest+`"`) {
			t.Errorf("install form does not post tag %s: %s", updLatest, form)
		}
		if strings.Contains(form, "disabled") {
			t.Errorf("install button is disabled: %s", form)
		}
		if !strings.Contains(form, `hx-boost="false"`) || !strings.Contains(form, "'primary'") {
			t.Errorf("install form is not a primary confirm form outside boost: %s", form)
		}
		if formHTML(body, rollback) != "" {
			t.Errorf("rollback form rendered without a previous version")
		}
		if strings.Contains(body, `hx-trigger="every 2s"`) {
			t.Errorf("an idle page polls the status")
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		c, i := newUpdateFakes()
		i.supported, i.reason = false, "systemd"
		body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
		if form := formHTML(body, install); !strings.Contains(form, "disabled") {
			t.Errorf("install button is not disabled: %q", form)
		}
		if !strings.Contains(body, "Krill is not running as a systemd service") {
			t.Errorf("page does not explain the systemd reason")
		}
		if i.busyCalls != 0 {
			t.Errorf("Busy queried %d times on an unsupported instance, want 0", i.busyCalls)
		}
	})

	t.Run("busy", func(t *testing.T) {
		c, i := newUpdateFakes()
		i.busy = "deploy"
		body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
		if form := formHTML(body, install); !strings.Contains(form, "disabled") {
			t.Errorf("install button is not disabled: %q", form)
		}
		if !strings.Contains(body, "Wait until this finishes: a deploy is running") {
			t.Errorf("page does not explain what is running")
		}
	})

	t.Run("up to date with a previous version", func(t *testing.T) {
		c, i := newUpdateFakes()
		c.newer = false
		i.previous = "v0.0.9"
		body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
		if formHTML(body, install) != "" {
			t.Errorf("install form rendered although nothing newer is available")
		}
		if !strings.Contains(body, "You are running the latest release.") {
			t.Errorf("page does not say it is up to date")
		}
		form := formHTML(body, rollback)
		if form == "" {
			t.Fatalf("no rollback form in the page")
		}
		if !strings.Contains(form, "Roll back to v0.0.9") || !strings.Contains(form, `hx-boost="false"`) || strings.Contains(form, "disabled") {
			t.Errorf("unexpected rollback form: %s", form)
		}
	})

	t.Run("job running", func(t *testing.T) {
		c, i := newUpdateFakes()
		i.previous = "v0.0.9"
		i.status = selfupdate.Status{Phase: selfupdate.PhaseRestarting, Tag: updLatest}
		body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
		if !strings.Contains(body, `hx-trigger="every 2s"`) || !strings.Contains(body, "If Krill does not start within 10 minutes") {
			t.Errorf("a running job does not render the polling status block")
		}
		if form := formHTML(body, install); !strings.Contains(form, "disabled") {
			t.Errorf("install button is not disabled while a job runs: %q", form)
		}
		if form := formHTML(body, rollback); !strings.Contains(form, "disabled") {
			t.Errorf("rollback button is not disabled while a job runs: %q", form)
		}
	})

	t.Run("failed job", func(t *testing.T) {
		c, i := newUpdateFakes()
		i.status = selfupdate.Status{Phase: selfupdate.PhaseFailed, Tag: updLatest, Err: errors.New("checksum mismatch")}
		body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
		for _, want := range []string{`id="k-update-status"`, "Failed", "The update failed: checksum mismatch"} {
			if !strings.Contains(body, want) {
				t.Errorf("page does not contain %q", want)
			}
		}
		if strings.Contains(body, "hx-trigger") {
			t.Errorf("a failed job still polls the status")
		}
		if form := formHTML(body, install); form == "" || strings.Contains(form, "disabled") {
			t.Errorf("install button is missing or disabled after a failed job: %q", form)
		}
	})

	// A job that gave up because work started while it downloaded says what,
	// in the same words as the refusal at the button.
	t.Run("job given up for work in flight", func(t *testing.T) {
		c, i := newUpdateFakes()
		i.status = selfupdate.Status{Phase: selfupdate.PhaseFailed, Tag: updLatest, Err: &selfupdate.BusyError{Reason: "deploy"}}
		body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
		if !strings.Contains(body, "The update failed: a deploy is running") {
			t.Errorf("page does not explain the busy failure")
		}
		if strings.Contains(body, "selfupdate: busy") {
			t.Errorf("page shows the raw busy error")
		}
	})

	t.Run("development build", func(t *testing.T) {
		// Available is forced on as well: the build kind alone must hide the
		// install card, whatever the checker says.
		for _, newer := range []bool{true, false} {
			c, i := newUpdateFakes()
			c.current, c.newer = "dev+abc", newer
			i.previous = "v0.0.9"
			body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
			if !strings.Contains(body, "This is a development build (dev+abc).") {
				t.Errorf("newer=%v: page does not say this is a development build", newer)
			}
			if strings.Contains(body, `name="tag"`) || formHTML(body, install) != "" {
				t.Errorf("newer=%v: a development build renders the install form", newer)
			}
			if strings.Contains(body, "You are running the latest release.") {
				t.Errorf("newer=%v: a development build claims to run the latest release", newer)
			}
			if formHTML(body, rollback) == "" {
				t.Errorf("newer=%v: the rollback form is gone on a development build", newer)
			}
		}
	})

	t.Run("last result", func(t *testing.T) {
		cases := []struct {
			last selfupdate.Result
			want string
		}{
			{selfupdate.Result{From: "v0.1.0", To: "v0.2.0", OK: true}, "Updated from v0.1.0 to v0.2.0."},
			{selfupdate.Result{From: "v0.2.0", To: "v0.1.0", OK: true, Reason: "rollback"}, "Rolled back from v0.2.0 to v0.1.0."},
			{selfupdate.Result{To: "v0.2.0", Reason: "reverted"}, "The update to v0.2.0 did not start and was rolled back."},
			{selfupdate.Result{To: "v0.2.0", Reason: "interrupted"}, "The update to v0.2.0 was interrupted before it was installed."},
		}
		for _, tc := range cases {
			c, i := newUpdateFakes()
			last := tc.last
			i.last = &last
			body := getPage(t, f.handler(c, i), base+"/updates", cookie, nil).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("result %+v: page does not contain %q", tc.last, tc.want)
			}
		}
	})

	t.Run("supported is cached", func(t *testing.T) {
		c, i := newUpdateFakes()
		h := f.handler(c, i)
		for n := 0; n < 2; n++ {
			if rec := getPage(t, h, base+"/updates", cookie, nil); rec.Code != http.StatusOK {
				t.Fatalf("GET /updates: want 200, got %d", rec.Code)
			}
		}
		if i.supportedCalls != 1 {
			t.Errorf("Supported called %d times for two page loads, want 1", i.supportedCalls)
		}
	})

	t.Run("not wired", func(t *testing.T) {
		h := f.handler(nil, nil)
		rec := getPage(t, h, base+"/updates", cookie, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Self-update is not enabled on this instance.") {
			t.Errorf("unwired page: want 200 explaining the feature is off, got %d", rec.Code)
		}
		for _, target := range []string{install, rollback, base + "/updates/check"} {
			rec := postForm(t, h, target, cookie, url.Values{"tag": {updLatest}})
			if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) || flashText(rec) != "Self-update is not enabled on this instance." {
				t.Errorf("POST %s unwired: want 303 + unwired error flash, got %d %q", target, rec.Code, flashCookieValue(rec))
			}
		}
		if rec := getPage(t, h, base+"/updates/status", cookie, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET /updates/status unwired: want 404, got %d", rec.Code)
		}

		// Half-wired is not wired either.
		_, i := newUpdateFakes()
		half := f.newServer()
		half.SetSelfUpdate(nil, i)
		rec = getPage(t, half.Router(), base+"/updates", cookie, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Self-update is not enabled on this instance.") {
			t.Errorf("half-wired page: want 200 explaining the feature is off, got %d", rec.Code)
		}
		if n := i.callCount(); n != 0 {
			t.Errorf("half-wired server called the installer %d times, want 0", n)
		}
	})
}

func TestUpdateActions(t *testing.T) {
	f := newUpdateFixture(t)
	base, cookie, _ := nodesOrg(t, f.q, f.orgSvc, "upd-actions@k.local", "OrgUpdAct")
	c, i := newUpdateFakes()
	h := f.handler(c, i)

	t.Run("install refusals", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
			want string
		}{
			{"unsupported", fmt.Errorf("%w: systemd", selfupdate.ErrUnsupported), "This instance can't update itself; see the page for why."},
			{"job running", selfupdate.ErrJobRunning, "An update is already in progress."},
			{"invalid tag", selfupdate.ErrInvalidTag, "The latest release changed. Check again, then update."},
			{"not latest", selfupdate.ErrNotLatest, "The latest release changed. Check again, then update."},
			{"not newer", selfupdate.ErrNotNewer, "That release is not newer than the running version."},
			{"pending", selfupdate.ErrPending, "The previous update is still waiting to confirm. Try again in a few minutes."},
			{"busy", &selfupdate.BusyError{Reason: "backup"}, "Not now: a database backup or restore is running."},
			{"unexpected", errors.New("disk on fire"), "Could not start the update."},
		}
		for _, tc := range cases {
			i.startErr = tc.err
			rec := postForm(t, h, base+"/updates/install", cookie, url.Values{"tag": {updLatest}})
			if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
				t.Errorf("%s: want 303 + error flash, got %d %q", tc.name, rec.Code, flashCookieValue(rec))
				continue
			}
			if flashText(rec) != tc.want {
				t.Errorf("%s: flash = %q, want %q", tc.name, flashText(rec), tc.want)
			}
		}
	})

	t.Run("install", func(t *testing.T) {
		i.startErr = nil
		before := i.startCalls
		rec := postForm(t, h, base+"/updates/install", cookie, url.Values{"tag": {updLatest}})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != base+"/updates" {
			t.Fatalf("install: want 303 to %s/updates, got %d %q", base, rec.Code, rec.Header().Get("Location"))
		}
		if flashText(rec) != "The update has started." || hasErrFlash(rec) {
			t.Errorf("install: flash = %q", flashCookieValue(rec))
		}
		if i.startCalls != before+1 || i.startTag != updLatest {
			t.Errorf("Start calls = %d (tag %q), want one more with %s", i.startCalls-before, i.startTag, updLatest)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		i.rollbackErr = selfupdate.ErrNoPrevious
		rec := postForm(t, h, base+"/updates/rollback", cookie, nil)
		if rec.Code != http.StatusSeeOther || flashText(rec) != "There is no previous version to roll back to." {
			t.Errorf("rollback without a previous: want 303 + no-previous flash, got %d %q", rec.Code, flashCookieValue(rec))
		}
		i.rollbackErr = nil
		i.status = selfupdate.Status{Phase: selfupdate.PhaseInstalling, Tag: "v0.0.9"}
		rec = postForm(t, h, base+"/updates/rollback", cookie, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != base+"/updates" || flashText(rec) != "The rollback has started." {
			t.Errorf("rollback: want 303 + ok flash, got %d %q %q", rec.Code, rec.Header().Get("Location"), flashCookieValue(rec))
		}
		if i.rollbackCalls != 2 {
			t.Errorf("Rollback calls = %d, want 2", i.rollbackCalls)
		}
	})

	t.Run("check", func(t *testing.T) {
		c.checkErr = selfupdate.ErrThrottled
		rec := postForm(t, h, base+"/updates/check", cookie, nil)
		if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
			t.Errorf("throttled check: want 303 + error flash, got %d %q", rec.Code, flashCookieValue(rec))
		}
		if c.checkCalls != 1 {
			t.Errorf("CheckNow calls = %d, want 1", c.checkCalls)
		}

		c.checkErr = selfupdate.ErrNoRelease
		if rec := postForm(t, h, base+"/updates/check", cookie, nil); flashText(rec) != "The repository has no published release." {
			t.Errorf("no release: flash = %q", flashCookieValue(rec))
		}

		c.checkErr = errors.New("unexpected status 500 from releases/latest")
		if rec := postForm(t, h, base+"/updates/check", cookie, nil); flashText(rec) != "Could not check for updates: unexpected status 500 from releases/latest" {
			t.Errorf("failed check: flash = %q", flashCookieValue(rec))
		}

		c.checkErr = nil
		rec = postForm(t, h, base+"/updates/check", cookie, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != base+"/updates" || flashText(rec) != "Checked for updates." {
			t.Errorf("check: want 303 + ok flash, got %d %q %q", rec.Code, rec.Header().Get("Location"), flashCookieValue(rec))
		}
		if c.checkCalls != 4 {
			t.Errorf("CheckNow calls = %d, want 4", c.checkCalls)
		}
	})
}

func TestUpdateStatus(t *testing.T) {
	f := newUpdateFixture(t)
	base, cookie, _ := nodesOrg(t, f.q, f.orgSvc, "upd-status@k.local", "OrgUpdStatus")
	htmx := http.Header{"Hx-Request": {"true"}}

	t.Run("another version answers with a refresh", func(t *testing.T) {
		c, i := newUpdateFakes()
		i.status = selfupdate.Status{Phase: selfupdate.PhaseRestarting, Tag: updLatest}
		rec := getPage(t, f.handler(c, i), base+"/updates/status?from=v0.0.1", cookie, htmx)
		if rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "true" || rec.Body.Len() != 0 {
			t.Errorf("want 200 + HX-Refresh and no body, got %d %q %q", rec.Code, rec.Header().Get("HX-Refresh"), rec.Body.String())
		}
	})

	t.Run("an active job keeps polling", func(t *testing.T) {
		c, i := newUpdateFakes()
		c.current = "dev+abc"
		i.status = selfupdate.Status{Phase: selfupdate.PhaseDownloading, Tag: updLatest}
		rec := getPage(t, f.handler(c, i), base+"/updates/status?from="+url.QueryEscape("dev+abc"), cookie, htmx)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "" {
			t.Fatalf("want 200 without HX-Refresh, got %d %q", rec.Code, rec.Header().Get("HX-Refresh"))
		}
		for _, want := range []string{`id="k-update-status"`, `hx-trigger="every 2s"`, `hx-swap="outerHTML"`,
			`hx-get="` + base + `/updates/status?from=dev%2Babc"`, "Downloading", "Version " + updLatest} {
			if !strings.Contains(body, want) {
				t.Errorf("fragment does not contain %q: %s", want, body)
			}
		}
	})

	// A job that is no longer active on the same version — failed here, or
	// idle after a restart that came back on the old binary (reverted,
	// interrupted) — refreshes the whole page: the result card and the
	// re-enabled buttons live outside the fragment.
	for _, tc := range []struct {
		name   string
		status selfupdate.Status
	}{
		{"an idle installer answers with a refresh", selfupdate.Status{Phase: selfupdate.PhaseIdle}},
		{"a failed job answers with a refresh", selfupdate.Status{Phase: selfupdate.PhaseFailed, Tag: updLatest, Err: errors.New("checksum mismatch")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, i := newUpdateFakes()
			i.status = tc.status
			rec := getPage(t, f.handler(c, i), base+"/updates/status?from="+updCurrent, cookie, htmx)
			if rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "true" || rec.Body.Len() != 0 {
				t.Errorf("want 200 + HX-Refresh and no body, got %d %q %q", rec.Code, rec.Header().Get("HX-Refresh"), rec.Body.String())
			}
		})
	}
}

func TestUpdateBadge(t *testing.T) {
	f := newUpdateFixture(t)
	adminBase, adminCookie, _ := nodesOrg(t, f.q, f.orgSvc, "upd-badge@k.local", "OrgUpdBadge")
	ownerBase, ownerCookie := f.plainOwner(t, "upd-badge-owner@k.local")

	c, i := newUpdateFakes()
	h := f.handler(c, i)
	if body := getPage(t, h, adminBase+"/registries", adminCookie, nil).Body.String(); !strings.Contains(body, "k-nav-badge") {
		t.Errorf("instance admin with an update available: no badge in the sidebar")
	}
	if body := getPage(t, h, ownerBase+"/registries", ownerCookie, nil).Body.String(); strings.Contains(body, "k-nav-badge") {
		t.Errorf("plain owner: the sidebar shows the update badge")
	}

	c.newer = false
	if body := getPage(t, h, adminBase+"/registries", adminCookie, nil).Body.String(); strings.Contains(body, "k-nav-badge") {
		t.Errorf("instance admin without an update: the sidebar shows the badge")
	}

	c.newer = true
	if body := getPage(t, f.handler(nil, nil), adminBase+"/registries", adminCookie, nil).Body.String(); strings.Contains(body, "k-nav-badge") {
		t.Errorf("unwired server: the sidebar shows the badge")
	}
}
