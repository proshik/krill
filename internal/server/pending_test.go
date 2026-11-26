package server_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// pendingEngine resolves every tag to one digest (counting the lookups, so an
// apply can be shown not to resolve its tag) and can report the app stopped.
type pendingEngine struct {
	noopEngine
	resolves atomic.Int32
	stopped  atomic.Bool
	image    atomic.Value // the image of the last ServiceDeploy
	live     atomic.Value // when set, the image the service runs instead
}

func (e *pendingEngine) ServiceDeploy(_ context.Context, s docker.ServiceSpec) error {
	e.image.Store(s.Image)
	return nil
}
func (e *pendingEngine) ServiceProgress(context.Context, string, []string) (docker.ServiceProgress, error) {
	p := docker.ServiceProgress{Found: true, Running: 1, Desired: 1}
	if e.stopped.Load() {
		p.Running, p.Desired = 0, 0
	}
	if img, ok := e.image.Load().(string); ok {
		p.Image = img
	}
	if img, ok := e.live.Load().(string); ok && img != "" {
		p.Image = img
	}
	return p, nil
}

const pendingDigest = "nginx@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func (e *pendingEngine) ResolveDigest(context.Context, string, string) (string, error) {
	e.resolves.Add(1)
	return pendingDigest, nil
}
func (e *pendingEngine) state() docker.ServiceState {
	if e.stopped.Load() {
		return docker.ServiceState{Found: true, Desired: 0}
	}
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}
}
func (e *pendingEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return e.state(), nil
}
func (e *pendingEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	m := map[string]docker.ServiceState{}
	for _, n := range names {
		m[n] = e.state()
	}
	return m, nil
}

func waitDeployment(t *testing.T, q *db.Queries, id int64, status string) db.Deployment {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, _ := q.GetDeployment(t.Context(), id)
		if row.Status == status {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment %d is %q, want %q", id, row.Status, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var (
	pendingMarker = regexp.MustCompile(`id="app-pending"><span class="state state--warn"[^>]*>.*?changes not applied`)
	applyButton   = `form="apply-form"`
)

// tabDot reports whether the sheet's tab named label carries the pending dot.
func tabDot(body, label string) bool {
	nav := body[strings.Index(body, `<nav class="seg" aria-label="web"`):]
	nav = nav[:strings.Index(nav, "</nav>")]
	for _, a := range strings.Split(nav, "</a>") {
		if strings.Contains(a, ">"+label) || strings.Contains(a, label+"<span") {
			return strings.Contains(a, `class="tab-dot"`)
		}
	}
	return false
}

func canvasDot(body string, appID int64) bool {
	i := strings.Index(body, `id="cp-s-app-`+i64(appID)+`"`)
	if i < 0 {
		return false
	}
	return strings.HasPrefix(body[i+len(`id="cp-s-app-`+i64(appID)+`"`):], `><span class="pending-dot"`)
}

func TestPendingChangesOnTheAppSheet(t *testing.T) {
	eng := &pendingEngine{}
	h, q, orgSvc, pool, dep := buildDeployServer(t, nil, eng)
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "pending.test")
	ctx := t.Context()
	page := func(c *http.Cookie, tab string) string {
		t.Helper()
		target := base
		if tab != "" {
			target += "?tab=" + tab
		}
		rec := getWithCookie(t, h, target, c)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", target, rec.Code)
		}
		return rec.Body.String()
	}
	setEnv := func(text string) {
		t.Helper()
		if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: appID, EnvText: text, EnvPublicKeys: []string{}}); err != nil {
			t.Fatal(err)
		}
	}

	// Never deployed: nothing.
	if body := page(cookie, ""); pendingMarker.MatchString(body) {
		t.Fatal("marker on a never deployed app")
	}
	// Deployed by a release that recorded nothing: unknown is not changed.
	first := dep.Enqueue(appID, "manual")
	waitDeployment(t, q, first, "done")
	if _, err := pool.Exec(ctx, "UPDATE deployments SET applied_config = NULL WHERE id = $1", first); err != nil {
		t.Fatal(err)
	}
	setEnv("A=1")
	if body := page(cookie, ""); pendingMarker.MatchString(body) || strings.Contains(body, applyButton) {
		t.Fatal("marker for a deployment without a fingerprint")
	}

	// Deployed now, then a variable changes.
	second := dep.Enqueue(appID, "manual")
	waitDeployment(t, q, second, "done")
	if body := page(cookie, ""); pendingMarker.MatchString(body) {
		t.Fatal("marker right after a deploy")
	}
	setEnv("A=2")
	body := page(cookie, "")
	if !pendingMarker.MatchString(body) {
		t.Fatal("no header marker after a variable changed")
	}
	if !strings.Contains(body, "changed after this deploy: variables") {
		t.Fatal("the current card does not name the changed section")
	}
	if !strings.Contains(body, `class="pill pill--primary" title="deploy the saved settings`) || !strings.Contains(body, applyButton) {
		t.Fatal("apply settings is not the highlighted action")
	}
	if strings.Contains(body, `form="deploy-again" class="pill pill--primary"`) {
		t.Fatal("deploy again is highlighted while apply settings can do it")
	}
	if !tabDot(body, "variables") || tabDot(body, "settings") || tabDot(body, "deployments") {
		t.Fatal("tab dots: want variables only")
	}
	if !strings.Contains(body, `<span class="visually-hidden">, has changes not applied</span>`) {
		t.Fatal("the tab dot has no accessible name")
	}
	if !canvasDot(body, appID) {
		t.Fatal("no marker on the canvas card")
	}
	// The deployments poll refreshes the header, tabs and canvas marker.
	poll := getWithCookie(t, h, base+"/deployments-list", cookie).Body.String()
	for _, oob := range []string{`id="app-pending" hx-swap-oob="true">`, `id="tab-dot-variables" hx-swap-oob="true">`, `id="cp-s-app-` + i64(appID) + `" hx-swap-oob="true">`} {
		if !strings.Contains(poll, oob) {
			t.Fatalf("poll lacks %s", oob)
		}
	}

	// A member sees what is not applied but gets no action, and cannot post it.
	member := mkUser(t, q, "pending-member@k.local")
	chain, _ := q.GetApplicationChain(ctx, appID)
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: chain.OrgID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	memberCookie := loginAs(t, q, "pending-member@k.local")
	if mb := page(memberCookie, ""); !pendingMarker.MatchString(mb) || strings.Contains(mb, applyButton+` class`) {
		t.Fatal("member: want the marker, no apply button")
	}
	if rec := postForm(t, h, base+"/apply-settings", memberCookie, url.Values{}); rec.Code != http.StatusForbidden {
		t.Fatalf("member apply = %d, want 403", rec.Code)
	}
	// Another organization's owner gets 404 for this app.
	mkUser(t, q, "pending-foreign@k.local")
	foreignOwner, _ := q.GetUserByEmail(ctx, "pending-foreign@k.local")
	foreignOrg, err := orgSvc.CreateOrg(ctx, foreignOwner.ID, "Foreign pending org")
	if err != nil {
		t.Fatal(err)
	}
	foreignPath := strings.Replace(base, "/orgs/"+i64(chain.OrgID)+"/", "/orgs/"+i64(foreignOrg.ID)+"/", 1) + "/apply-settings"
	if rec := postForm(t, h, foreignPath, loginAs(t, q, "pending-foreign@k.local"), url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-organization apply = %d, want 404", rec.Code)
	}

	// A deploy in flight hides it.
	running, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: appID, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if body := page(cookie, ""); pendingMarker.MatchString(body) || canvasDot(body, appID) {
		t.Fatal("marker while a deploy is in flight")
	}
	// An open canvas drops the marker at its next status poll.
	envPoll := base[:strings.Index(base, "/apps/")] + "/statuses"
	if poll := getWithCookie(t, h, envPoll, cookie).Body.String(); !strings.Contains(poll, `id="cp-s-app-`+i64(appID)+`" hx-swap-oob="true"></span>`) {
		t.Fatal("the canvas poll keeps the marker of an app being deployed")
	}
	if err := q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: running.ID, Status: "error", ErrorMessage: "test"}); err != nil {
		t.Fatal(err)
	}

	// The canvas status poll looks nothing up and leaves a running app's marker.
	envBase := base[:strings.Index(base, "/apps/")]
	if poll := getWithCookie(t, h, envBase+"/statuses", cookie).Body.String(); strings.Contains(poll, `id="cp-s-app-`) {
		t.Fatal("the status poll touched the marker of a running app")
	}

	// History naming another image than the one that runs: no apply offered.
	eng.live.Store("nginx@sha256:" + strings.Repeat("c", 64))
	if body := page(cookie, ""); !pendingMarker.MatchString(body) || strings.Contains(body, applyButton+` class`) || !strings.Contains(body, `form="deploy-again" class="pill pill--primary"`) {
		t.Fatal("another live image: want the marker, no apply button, deploy again highlighted")
	}
	if rec := postForm(t, h, base+"/apply-settings", cookie, url.Values{}); !hasErrFlash(rec) {
		t.Fatal("apply accepted while the service runs another image")
	}
	eng.live.Store("")
	if poll := getWithCookie(t, h, base+"/status", cookie).Body.String(); strings.Contains(poll, "hx-swap-oob") {
		t.Fatal("the state poll of a running app touches its markers")
	}

	// A stopped app shows nothing and cannot apply.
	eng.stopped.Store(true)
	// The sheet's state poll clears the header marker and the tab dots.
	if poll := getWithCookie(t, h, base+"/status", cookie).Body.String(); !strings.Contains(poll, `id="app-pending" hx-swap-oob="true"></span>`) || !strings.Contains(poll, `id="tab-dot-variables" hx-swap-oob="true"></span>`) {
		t.Fatal("the state poll does not clear the markers of a stopped app")
	}
	if body := page(cookie, ""); pendingMarker.MatchString(body) || canvasDot(body, appID) {
		t.Fatal("marker on a stopped app")
	}
	// The status poll clears the marker an open canvas still shows.
	if poll := getWithCookie(t, h, envBase+"/statuses", cookie).Body.String(); !strings.Contains(poll, `id="cp-s-app-`+i64(appID)+`" hx-swap-oob="true"></span>`) {
		t.Fatal("the status poll does not clear the marker of a stopped app")
	}
	before, _ := q.ListDeploymentSummariesByApplication(ctx, appID)
	if rec := postForm(t, h, base+"/apply-settings", cookie, url.Values{}); !hasErrFlash(rec) {
		t.Fatalf("apply on a stopped app: %d %q", rec.Code, flashCookieValue(rec))
	}
	if after, _ := q.ListDeploymentSummariesByApplication(ctx, appID); len(after) != len(before) {
		t.Fatal("apply on a stopped app created a deployment")
	}
	eng.stopped.Store(false)

	// Apply settings: the pinned image, no tag lookup, no build; then nothing pending.
	resolves := eng.resolves.Load()
	rec := postForm(t, h, base+"/apply-settings", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("apply = %d %q", rec.Code, flashCookieValue(rec))
	}
	deps, _ := q.ListDeploymentSummariesByApplication(ctx, appID)
	applied := waitDeployment(t, q, deps[0].ID, "done")
	if applied.ImageTag != pendingDigest || applied.Trigger != "manual" || eng.resolves.Load() != resolves {
		t.Fatalf("apply deployed %q by %s with %d tag lookups", applied.ImageTag, applied.Trigger, eng.resolves.Load()-resolves)
	}
	if body := page(cookie, ""); pendingMarker.MatchString(body) || tabDot(body, "variables") || canvasDot(body, appID) {
		t.Fatal("markers left after apply settings")
	}
	if rec := postForm(t, h, base+"/apply-settings", cookie, url.Values{}); !hasErrFlash(rec) {
		t.Fatal("a second apply with nothing pending was accepted")
	}

	// A changed tag needs a deploy: apply is hidden, deploy again highlighted.
	if err := q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{ID: appID, Image: "nginx", Tag: "next"}); err != nil {
		t.Fatal(err)
	}
	body = page(cookie, "")
	if !strings.Contains(body, "changed after this deploy: source") || strings.Contains(body, applyButton+` class`) {
		t.Fatal("a changed tag: want the source named and no apply button")
	}
	if !strings.Contains(body, `form="deploy-again" class="pill pill--primary"`) {
		t.Fatal("a changed tag: deploy again is not highlighted")
	}
	if !tabDot(body, "settings") || tabDot(body, "variables") {
		t.Fatal("a changed tag: want the settings tab dot only")
	}
	if rec := postForm(t, h, base+"/apply-settings", cookie, url.Values{}); !hasErrFlash(rec) {
		t.Fatal("apply after a tag change was accepted")
	}
}
