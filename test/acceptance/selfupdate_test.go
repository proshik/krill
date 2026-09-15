//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stageDir is where binaries and install.sh are copied to inside the VM.
const stageDir = "/tmp/krill-accept"

func TestMain(m *testing.M) {
	if os.Getenv("KRILL_ACCEPT") != "1" {
		fmt.Println("skipping the self-update acceptance tests: set KRILL_ACCEPT=1 (see docs/development.md)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// suite is the state the ordered steps share. The VM state is cumulative, so
// a failed step stops the chain.
type suite struct {
	ctx    context.Context
	cfg    Config
	vm     *VM
	host   *Host
	client *Client
	orgID  int64
	rels   map[string]Release
	app    appRef
}

// appRef locates the image app the busy scenarios deploy.
type appRef struct {
	projID, envID, appID int64
}

func (a appRef) path(orgID int64) string {
	return fmt.Sprintf("/orgs/%d/projects/%d/environments/%d/apps/%d", orgID, a.projID, a.envID, a.appID)
}

func newSuite(t *testing.T) *suite {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	vm := NewVM(t, cfg)
	return &suite{
		ctx:    context.Background(),
		cfg:    cfg,
		vm:     vm,
		host:   NewHost(vm),
		client: NewClient(t, cfg.HostPort),
		rels:   map[string]Release{},
	}
}

// run runs one step and stops the whole chain when it fails.
func (s *suite) run(t *testing.T, name string, fn func(t *testing.T, ev *Evidence)) {
	t.Helper()
	ok := t.Run(name, func(t *testing.T) {
		ev := NewEvidence(t, s.cfg, t.Name())
		fn(t, ev)
	})
	if !ok {
		t.Logf("step %s failed; stopping (the VM %q is kept for inspection)", name, s.cfg.VM)
		t.FailNow()
	}
}

// must fails the step on err.
func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// sudo runs a root command, records it and fails the step on error.
func (s *suite) sudo(t *testing.T, ev *Evidence, cmd string) string {
	t.Helper()
	out, err := s.vm.Sudo(s.ctx, cmd)
	ev.Output(cmd, out)
	must(t, err, cmd)
	return out
}

// buildReleases builds every variant and records the checksums.
func (s *suite) buildReleases(t *testing.T, ev *Evidence) {
	for _, v := range Variants {
		start := time.Now()
		rel, err := BuildRelease(s.ctx, t, s.cfg, v)
		must(t, err, "building "+v.Tag)
		sums, err := os.ReadFile(rel.Checksums)
		must(t, err, "reading checksums of "+v.Tag)
		ev.Note("built %s in %s: %s", v.Tag, time.Since(start).Round(time.Second), rel.Binary)
		ev.Output("cat "+rel.Checksums, string(sums))
		s.rels[v.Tag] = rel
	}
	main := filepath.Join(s.rels["v0.90.2"].Source, "cmd", "krill", "main.go")
	out, err := runHost(s.ctx, t, "", nil, "grep", "-n", "-B4", "-A1", "-F", "simulated crash after migrations", main)
	must(t, err, "grepping the v0.90.2 patch")
	ev.Output("grep -n -B4 -A1 'simulated crash after migrations' src-v0.90.2/cmd/krill/main.go", out)
}

// stage copies a file into the VM's stage directory.
func (s *suite) stage(t *testing.T, ev *Evidence, local, name string) string {
	t.Helper()
	_, err := s.vm.Exec(s.ctx, "mkdir -p "+stageDir)
	must(t, err, "creating the stage directory")
	remote := stageDir + "/" + name
	must(t, s.vm.CopyIn(s.ctx, local, remote), "copying "+local)
	_, err = s.vm.Exec(s.ctx, "chmod 0755 "+shQuote(remote))
	must(t, err, "chmod "+remote)
	ev.Note("copied %s -> %s:%s", local, s.cfg.VM, remote)
	return remote
}

// stageInstaller copies install.sh from the exported source of rel.
func (s *suite) stageInstaller(t *testing.T, ev *Evidence, rel Release) string {
	return s.stage(t, ev, filepath.Join(rel.Source, "install.sh"), "install.sh")
}

// runInstaller runs install.sh with KRILL_BINARY on the VM.
func (s *suite) runInstaller(t *testing.T, ev *Evidence, rel Release) {
	t.Helper()
	installer := s.stageInstaller(t, ev, rel)
	bin := s.stage(t, ev, rel.Binary, "krill-"+rel.Tag)
	cmd := "KRILL_BINARY=" + shQuote(bin) + " sh " + shQuote(installer)
	start := time.Now()
	out, err := s.vm.Sudo(s.ctx, cmd)
	ev.Output("sudo "+cmd, out)
	ev.Note("install.sh took %s", time.Since(start).Round(time.Second))
	must(t, err, "install.sh")
}

// resetInstall removes a previous Krill install from the VM, following the
// Uninstall section of docs/install.md, plus the Swarm services an earlier
// run created, so every run starts from a fresh install on schema 46.
func (s *suite) resetInstall(t *testing.T, ev *Evidence) {
	t.Helper()
	script := strings.Join([]string{
		"systemctl stop krill-update-revert.timer krill-update-restart.timer 2>/dev/null || true",
		"systemctl disable --now krill 2>/dev/null || true",
		"rm -f /etc/systemd/system/krill.service /usr/local/bin/krill /usr/local/bin/krill.prev",
		"rm -f /etc/systemd/system/krill.service.d/10-krill-update.conf",
		"rmdir /etc/systemd/system/krill.service.d 2>/dev/null || true",
		"systemctl daemon-reload",
		"systemctl reset-failed krill krill-update-revert.timer krill-update-revert.service krill-update-restart.timer krill-update-restart.service 2>/dev/null || true",
		"rm -rf /etc/krill",
		"rm -f /run/krill-update-pending /run/krill-update-reverted /run/krill-update-rolledback",
		"if command -v docker >/dev/null 2>&1; then " +
			"docker service ls --format '{{.Name}}' | grep '^krill-' | xargs -r docker service rm; " +
			"docker rm -f krill-postgres 2>/dev/null || true; " +
			"for i in 1 2 3 4 5 6 7 8 9 10; do docker volume rm krill-pg-data >/dev/null 2>&1 && break; " +
			"docker volume inspect krill-pg-data >/dev/null 2>&1 || break; sleep 1; done; " +
			"fi",
		"echo reset-done",
	}, "\n")
	s.sudo(t, ev, script)
}

// setUpdateRepo points the installed Krill at the test repository. install.sh
// does not know KRILL_UPDATE_REPO, so it goes into krill.env afterwards.
func (s *suite) setUpdateRepo(t *testing.T, ev *Evidence) {
	t.Helper()
	repo := s.cfg.Repo
	s.sudo(t, ev, fmt.Sprintf("if grep -q '^KRILL_UPDATE_REPO=' %[1]s; then sed -i 's#^KRILL_UPDATE_REPO=.*#KRILL_UPDATE_REPO=%[2]s#' %[1]s; "+
		"else echo 'KRILL_UPDATE_REPO=%[2]s' >> %[1]s; fi; grep '^KRILL_UPDATE_REPO=' %[1]s; systemctl restart krill", envFile, repo))
}

// installFresh resets the VM and installs rel with install.sh, then waits
// until it serves and signs in.
func (s *suite) installFresh(t *testing.T, ev *Evidence, rel Release) {
	t.Helper()
	s.resetInstall(t, ev)
	s.runInstaller(t, ev, rel)
	s.setUpdateRepo(t, ev)
	s.waitServing(t, ev, 10*time.Minute)
	s.login(t, ev)
	org, err := s.host.DefaultOrgID(s.ctx)
	must(t, err, "resolving the default organization")
	s.orgID = org
	ev.Note("default organization id %d", org)
}

func (s *suite) waitServing(t *testing.T, ev *Evidence, timeout time.Duration) bool {
	t.Helper()
	start := time.Now()
	saw, err := s.client.WaitServing(s.ctx, timeout)
	ev.Note("serving on 127.0.0.1:%d after %s (starting page seen: %v)", s.cfg.HostPort, time.Since(start).Round(time.Second), saw)
	must(t, err, "waiting for krill to serve")
	return saw
}

func (s *suite) login(t *testing.T, ev *Evidence) {
	t.Helper()
	email, err := s.host.AdminEmail(s.ctx)
	must(t, err, "reading the admin email")
	pw, err := s.host.AdminPassword(s.ctx)
	must(t, err, "reading the admin password")
	must(t, s.client.Login(s.ctx, email, pw), "login")
	ev.Note("logged in as %s", email)
}

// expectVersion asserts the installed binary's version.
func (s *suite) expectVersion(t *testing.T, ev *Evidence, want string) {
	t.Helper()
	got, err := s.host.KrillVersion(s.ctx)
	must(t, err, "krill --version")
	ev.Note("krill --version: %s", got)
	if got != want {
		t.Fatalf("installed version %s, want %s", got, want)
	}
}

// expectSchema asserts a clean schema at want.
func (s *suite) expectSchema(t *testing.T, ev *Evidence, want int) {
	t.Helper()
	v, dirty, err := s.host.SchemaVersion(s.ctx)
	must(t, err, "reading schema_migrations")
	ev.Note("schema_migrations: version=%d dirty=%v", v, dirty)
	if v != want || dirty {
		t.Fatalf("schema version %d dirty=%v, want %d clean", v, dirty, want)
	}
}

func (s *suite) expectPrev(t *testing.T, ev *Evidence, want string) {
	t.Helper()
	got, ok, err := s.host.PrevVersion(s.ctx)
	must(t, err, "reading krill.prev")
	ev.Note("krill.prev: present=%v version=%s", ok, got)
	switch {
	case want == "" && ok:
		t.Fatalf("krill.prev is %s, want none", got)
	case want != "" && got != want:
		t.Fatalf("krill.prev is %q (present=%v), want %s", got, ok, want)
	}
}

func (s *suite) expectUnitActive(t *testing.T, ev *Evidence) {
	t.Helper()
	st, err := s.host.UnitState(s.ctx)
	must(t, err, "systemctl show krill")
	ev.Output("systemctl show krill -p ActiveState,SubState,NRestarts,Result", st.Raw)
	if st.ActiveState != "active" {
		t.Fatalf("krill unit is %s/%s, want active", st.ActiveState, st.SubState)
	}
}

// page fetches the Updates page and records its text.
func (s *suite) page(t *testing.T, ev *Evidence) string {
	t.Helper()
	resp, err := s.client.UpdatesPage(s.ctx, s.orgID)
	must(t, err, "updates page")
	text := resp.Text()
	ev.Output("GET "+UpdatesPath(s.orgID)+" (text)", text)
	return text
}

// waitPage polls the Updates page until it contains every want, re-signing
// in when a restart lost nothing but the page is not reachable yet.
func (s *suite) waitPage(t *testing.T, ev *Evidence, timeout time.Duration, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		resp, err := s.client.Get(s.ctx, UpdatesPath(s.orgID))
		if err == nil && resp.Status == http.StatusOK {
			last = resp.Text()
			all := true
			for _, w := range want {
				if !strings.Contains(last, w) {
					all = false
					break
				}
			}
			if all {
				ev.Output("GET "+UpdatesPath(s.orgID)+" (text)", last)
				return last
			}
		}
		if time.Now().After(deadline) {
			ev.Output("GET "+UpdatesPath(s.orgID)+" (last text)", last)
			t.Fatalf("updates page never showed %q within %s", want, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// journalCursor records the journal position before an action.
func (s *suite) journalCursor(t *testing.T) string {
	t.Helper()
	c, err := s.host.JournalCursor(s.ctx)
	must(t, err, "journal cursor")
	return c
}

func (s *suite) journal(t *testing.T, ev *Evidence, cursor string) string {
	t.Helper()
	out, err := s.host.JournalSince(s.ctx, cursor)
	must(t, err, "journalctl")
	ev.Output("journalctl -u krill --after-cursor=<step start>", out)
	return out
}

// latest steers the test repository's latest release (writes to GitHub) and
// waits until Krill offers it.
func (s *suite) latest(t *testing.T, ev *Evidence, tag string) {
	t.Helper()
	must(t, SetLatest(s.ctx, t, s.cfg, tag), "gh release edit --latest "+tag)
	ev.Note("gh release edit %s --repo %s --latest", tag, s.cfg.Repo)
	s.waitAvailable(t, ev, tag)
}

func (s *suite) waitAvailable(t *testing.T, ev *Evidence, tag string) {
	t.Helper()
	start := time.Now()
	resp, err := s.client.WaitAvailable(s.ctx, s.orgID, tag, 6*time.Minute)
	ev.Note("Check now offered %s after %s", tag, time.Since(start).Round(time.Second))
	must(t, err, "waiting for "+tag)
	_ = resp
}

// flashContains asserts a POST's resulting page carries a flash text.
func flashContains(t *testing.T, ev *Evidence, what string, resp Response, want string) {
	t.Helper()
	ev.Output(what+" -> "+strconv.Itoa(resp.Status)+" "+resp.URL+" (text)", resp.Text())
	if !strings.Contains(resp.Text(), want) {
		t.Fatalf("%s: page does not say %q", what, want)
	}
}

// expectNoJobTrace asserts an update left nothing behind on the host.
func (s *suite) expectNoJobTrace(t *testing.T, ev *Evidence) {
	t.Helper()
	s.expectPrev(t, ev, "")
	drop, err := s.host.DropInPresent(s.ctx)
	must(t, err, "drop-in")
	pending, err := s.host.FileExists(s.ctx, pendingMarker)
	must(t, err, "pending marker")
	revert, err := s.host.TimerActive(s.ctx, revertTimer)
	must(t, err, "revert timer")
	restart, err := s.host.TimerActive(s.ctx, restartTimer)
	must(t, err, "restart timer")
	timers, err := s.host.Timers(s.ctx)
	must(t, err, "list-timers")
	ev.Output("systemctl list-timers --all --no-legend 'krill-update-*'", timers)
	ev.Note("drop-in=%v pending-marker=%v revert-timer-active=%v restart-timer-active=%v", drop, pending, revert, restart)
	if drop || pending || revert || restart {
		t.Fatalf("the refused update changed the host: drop-in=%v pending=%v revert-timer=%v restart-timer=%v", drop, pending, revert, restart)
	}
}

// --- app helpers (form posts as in scripts/e2e.sh) ---------------------------

func (s *suite) post(t *testing.T, ev *Evidence, path string, form url.Values) Response {
	t.Helper()
	resp, err := s.client.Post(s.ctx, path, path, form)
	must(t, err, "POST "+path)
	ev.Note("POST %s -> %d %s", path, resp.Status, resp.URL)
	if resp.Status != http.StatusOK {
		t.Fatalf("POST %s ended with status %d", path, resp.Status)
	}
	return resp
}

func (s *suite) queryID(t *testing.T, sql string) int64 {
	t.Helper()
	out, err := s.host.Psql(s.ctx, sql)
	must(t, err, sql)
	id, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	must(t, err, "parsing id from "+sql)
	return id
}

// createBusyApp creates a project, an environment and an nginx image app
// whose healthcheck never passes, so a deploy of it stays in flight for
// minutes (start period plus the convergence timeout).
func (s *suite) createBusyApp(t *testing.T, ev *Evidence) {
	t.Helper()
	org := fmt.Sprintf("/orgs/%d", s.orgID)
	s.post(t, ev, org+"/projects", url.Values{"name": {"accept"}, "description": {""}})
	s.app.projID = s.queryID(t, fmt.Sprintf("select id from projects where organization_id=%d and name='accept' order by id desc limit 1", s.orgID))
	s.post(t, ev, fmt.Sprintf("%s/projects/%d/environments", org, s.app.projID), url.Values{"name": {"production"}})
	s.app.envID = s.queryID(t, fmt.Sprintf("select id from environments where project_id=%d order by id desc limit 1", s.app.projID))
	s.post(t, ev, fmt.Sprintf("%s/projects/%d/environments/%d/apps", org, s.app.projID, s.app.envID), url.Values{
		"name": {"busyapp"}, "image": {"nginx"}, "tag": {"alpine"}, "domain": {""}, "port": {"80"}, "env": {""},
	})
	s.app.appID = s.queryID(t, fmt.Sprintf("select id from applications where environment_id=%d and name='busyapp'", s.app.envID))
	// The healthcheck command runs through CMD-SHELL; `false` always fails.
	s.post(t, ev, s.app.path(s.orgID)+"/advanced", url.Values{
		"command": {""}, "memory_limit": {""}, "cpu_limit": {""}, "replicas": {"1"},
		"restart_condition": {"any"}, "restart_max_attempts": {"0"},
		"healthcheck_cmd": {"false"}, "healthcheck_interval": {"5s"}, "healthcheck_timeout": {"3s"},
		"healthcheck_start_period": {"240s"}, "healthcheck_retries": {"1"},
	})
	ev.Note("app %+v", s.app)
}

func (s *suite) deploy(t *testing.T, ev *Evidence) {
	t.Helper()
	s.post(t, ev, s.app.path(s.orgID)+"/deploy", url.Values{"image": {"nginx"}, "tag": {"alpine"}})
	s.waitDeployStatus(t, ev, 60*time.Second, func(st string) bool { return st == "running" })
}

func (s *suite) deployStatus(t *testing.T) string {
	t.Helper()
	out, err := s.host.Psql(s.ctx, fmt.Sprintf("select status from deployments where application_id=%d order by started_at desc limit 1", s.app.appID))
	must(t, err, "deployment status")
	return out
}

func (s *suite) waitDeployStatus(t *testing.T, ev *Evidence, timeout time.Duration, ok func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	start := time.Now()
	for {
		st := s.deployStatus(t)
		if ok(st) {
			ev.Note("latest deployment of app %d is %q after %s", s.app.appID, st, time.Since(start).Round(time.Second))
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("latest deployment of app %d still %q after %s", s.app.appID, st, timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

func (s *suite) waitNoRunningDeploy(t *testing.T, ev *Evidence) {
	t.Helper()
	s.waitDeployStatus(t, ev, 15*time.Minute, func(st string) bool { return st != "running" })
}

// --- egress throttle ------------------------------------------------------------

// throttleScript slows the VM's traffic to 2 Mbit/s so a ~40 MB release
// download takes minutes. A root tbf only shapes what the VM sends; the
// download itself is inbound, so an ingress policer does the actual work.
const throttleScript = `set -e
IF=$(ip -o -4 route show default | awk '{print $5; exit}')
[ -n "$IF" ] || { echo "no default route interface"; exit 1; }
tc qdisc del dev "$IF" root 2>/dev/null || true
tc qdisc del dev "$IF" ingress 2>/dev/null || true
tc qdisc add dev "$IF" root tbf rate 2mbit burst 32kbit latency 400ms
tc qdisc add dev "$IF" handle ffff: ingress
tc filter add dev "$IF" parent ffff: protocol all prio 1 u32 match u32 0 0 police rate 2mbit burst 64k drop flowid :1
tc qdisc show dev "$IF"
tc filter show dev "$IF" parent ffff:`

const unthrottleScript = `IF=$(ip -o -4 route show default | awk '{print $5; exit}')
tc qdisc del dev "$IF" root 2>/dev/null || true
tc qdisc del dev "$IF" ingress 2>/dev/null || true
tc qdisc show dev "$IF"`

// --- restart observation --------------------------------------------------------

// restartObservation is what polling the status fragment across a restart saw.
type restartObservation struct {
	phases       map[string]bool
	sawStarting  bool
	connErrors   int
	firstDown    time.Duration
	firstServing time.Duration
}

// followRestart polls the status fragment (as the page's htmx does, 4 times a
// second) from the click until a process running another version than from
// serves the real router. A fragment answering HX-Refresh from the same
// version means the job ended without a restart: when the page then shows a
// failed job the step fails right away.
func (s *suite) followRestart(t *testing.T, ev *Evidence, from string, timeout time.Duration) restartObservation {
	t.Helper()
	obs := restartObservation{phases: map[string]bool{}}
	start := time.Now()
	deadline := start.Add(timeout)
	down := false
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		resp, err := s.client.Status(ctx, s.orgID, from)
		cancel()
		switch {
		case err != nil:
			obs.connErrors++
			if !down {
				down, obs.firstDown = true, time.Since(start)
			}
		case resp.Status == http.StatusServiceUnavailable:
			obs.sawStarting = true
			if !down {
				down, obs.firstDown = true, time.Since(start)
			}
		case resp.Status == http.StatusOK && !resp.HXRefresh:
			for _, p := range []string{"Downloading", "Verifying", "Installing", "Restarting"} {
				if strings.Contains(resp.Text(), p) {
					obs.phases[p] = true
				}
			}
		case resp.Status == http.StatusOK && resp.HXRefresh:
			page, perr := s.client.Get(s.ctx, UpdatesPath(s.orgID))
			if perr != nil || page.Status != http.StatusOK {
				break // restarting right now; poll again
			}
			text := page.Text()
			if strings.Contains(text, "Running version "+from) {
				if strings.Contains(text, "The update failed") {
					ev.Output("GET "+UpdatesPath(s.orgID)+" (text)", text)
					t.Fatalf("the job failed instead of restarting")
				}
				break // same process, the job is between phases; poll again
			}
			obs.firstServing = time.Since(start)
			ev.Note("restart observed: phases=%v starting-page=%v conn-errors=%d first-down-at=%s serving-at=%s",
				obs.phases, obs.sawStarting, obs.connErrors, obs.firstDown.Round(100*time.Millisecond), obs.firstServing.Round(100*time.Millisecond))
			return obs
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("no restart observed within %s (phases=%v, starting page=%v, conn errors=%d)", timeout, obs.phases, obs.sawStarting, obs.connErrors)
	return obs
}

// updateTo clicks Install for tag (already offered) and follows it to a
// confirmed new process.
func (s *suite) updateTo(t *testing.T, ev *Evidence, from, to string, schema int) {
	t.Helper()
	resp, err := s.client.Install(s.ctx, s.orgID, to)
	must(t, err, "install")
	flashContains(t, ev, "POST install "+to, resp, "The update has started.")
	obs := s.followRestart(t, ev, from, 12*time.Minute)
	if !obs.sawStarting {
		// Not a failure: the gate is up only while one small migration runs
		// and the unchanged gateway is checked, which can fit between two
		// polls. The evidence says how long the process was unreachable.
		ev.Note("the 503 starting page was NOT seen (polling every 250ms); unreachable from %s to %s after the click, %d connection errors",
			obs.firstDown.Round(100*time.Millisecond), obs.firstServing.Round(100*time.Millisecond), obs.connErrors)
	}
	s.waitServing(t, ev, 2*time.Minute)
	s.waitPage(t, ev, 2*time.Minute, "Running version "+to, "Updated from "+from+" to "+to+".")
	s.expectVersion(t, ev, to)
	s.expectPrev(t, ev, from)
	s.expectSchema(t, ev, schema)
	drop, err := s.host.DropInPresent(s.ctx)
	must(t, err, "drop-in")
	ev.Note("drop-in present: %v", drop)
	if !drop {
		t.Fatalf("drop-in %s missing after the update", dropInPath)
	}
	s.expectRevertTimerGone(t, ev, 30*time.Second)
	s.expectUnitActive(t, ev)
}

// expectRevertTimerGone waits for ConfirmStartup to cancel the revert timer.
func (s *suite) expectRevertTimerGone(t *testing.T, ev *Evidence, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		active, err := s.host.TimerActive(s.ctx, revertTimer)
		must(t, err, "revert timer state")
		if !active {
			timers, _ := s.host.Timers(s.ctx)
			ev.Output("systemctl list-timers --all --no-legend 'krill-update-*'", timers)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still active %s after the new process served", revertTimer, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitInstalled waits until the binary on disk reports tag (the swap
// happened), which for a throttled or slow download can take minutes.
func (s *suite) waitInstalled(t *testing.T, ev *Evidence, tag string, timeout time.Duration) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		v, err := s.host.KrillVersion(s.ctx)
		if err == nil && v == tag {
			ev.Note("binary on disk is %s after %s", tag, time.Since(start).Round(time.Second))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("binary on disk is %q (err %v), want %s after %s", v, err, tag, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// ---------------------------------------------------------------------------------
// Phase A: the VM part, no GitHub writes.
// ---------------------------------------------------------------------------------

// TestVMSmoke builds the releases, creates the VM, installs v0.90.0 with
// install.sh and checks that the Updates page reports the running version on
// a host where self-update is supported. It never writes to GitHub.
func TestVMSmoke(t *testing.T) {
	s := newSuite(t)

	s.run(t, "build_releases", s.buildReleases)

	s.run(t, "vm_up", func(t *testing.T, ev *Evidence) {
		start := time.Now()
		must(t, s.vm.Ensure(s.ctx), "creating/starting the VM")
		ev.Note("VM %s running after %s", s.cfg.VM, time.Since(start).Round(time.Second))
		// Arguments must reach bash intact through limactl shell.
		out, err := s.vm.Exec(s.ctx, `printf '%s|' "a b" 'c'; echo`)
		must(t, err, "quoting probe")
		ev.Output(`printf '%s|' "a b" 'c'`, out)
		if strings.TrimSpace(out) != "a b|c|" {
			t.Fatalf("limactl shell mangles arguments: got %q, want %q", strings.TrimSpace(out), "a b|c|")
		}
		s.sudo(t, ev, "uname -a; . /etc/os-release; echo $PRETTY_NAME; id -u; nproc; free -m; df -h /")
	})

	s.run(t, "install_v0.90.0", func(t *testing.T, ev *Evidence) {
		s.installFresh(t, ev, s.rels["v0.90.0"])
		s.expectVersion(t, ev, "v0.90.0")
		s.expectSchema(t, ev, 46)
		s.expectUnitActive(t, ev)
		s.sudo(t, ev, "cat /etc/systemd/system/krill.service; grep -v -e PASSWORD -e SECRET -e DATABASE_URL "+envFile)
	})

	s.run(t, "updates_page", func(t *testing.T, ev *Evidence) {
		text := s.page(t, ev)
		for _, want := range []string{"Running version v0.90.0", "Check now"} {
			if !strings.Contains(text, want) {
				t.Fatalf("updates page does not show %q", want)
			}
		}
		for _, bad := range []string{"can't update itself", "development build", "Self-update is not enabled"} {
			if strings.Contains(text, bad) {
				t.Fatalf("updates page says %q", bad)
			}
		}

		// Check now against a repository that has no release (or does not
		// exist yet): an error is expected here and only recorded.
		resp, err := s.client.CheckNow(s.ctx, s.orgID)
		must(t, err, "check now")
		ev.Output("POST check (text, expected to report a failed check before Phase B)", resp.Text())
		for _, marker := range []string{"Could not check for updates", "Last check failed", "has no published release", "Checked for updates."} {
			if strings.Contains(resp.Text(), marker) {
				ev.Note("check result contains %q", marker)
			}
		}

		// The page renders the unsupported reason only next to an update or
		// rollback offer, and there is none yet. Supported() is the first
		// thing Install checks, so posting a tag that is not the latest
		// release proves it: an unsupported host answers "can't update
		// itself", a supported one gets as far as "the latest release
		// changed" — and no job starts.
		resp, err = s.client.Install(s.ctx, s.orgID, "v0.0.1")
		must(t, err, "install probe")
		flashContains(t, ev, "POST install v0.0.1 (supported probe)", resp, "The latest release changed. Check again, then update.")
		if strings.Contains(resp.Text(), "can't update itself") {
			t.Fatalf("self-update is not supported on the VM")
		}
		s.expectNoJobTrace(t, ev)

		// The same conditions, read off the host.
		s.sudo(t, ev, `pid=$(systemctl show krill -p MainPID --value); echo "MainPID=$pid"; `+
			`echo "PPid=$(awk '/^PPid:/{print $2}' /proc/$pid/status) EUid=$(awk '/^Uid:/{print $3}' /proc/$pid/status)"; `+
			`tr '\0' '\n' </proc/$pid/environ | grep -c '^INVOCATION_ID=' ; readlink /proc/$pid/exe; grep krill /proc/$pid/cgroup || cat /proc/$pid/cgroup`)
	})

	s.run(t, "other_binaries_run_in_vm", func(t *testing.T, ev *Evidence) {
		for _, tag := range []string{"v0.90.1", "v0.90.2", "v0.90.3", "v0.90.4"} {
			remote := s.stage(t, ev, s.rels[tag].Binary, "krill-"+tag)
			got, err := s.host.BinaryVersion(s.ctx, remote)
			must(t, err, remote+" --version")
			ev.Note("%s --version: krill %s", remote, got)
			if got != tag {
				t.Fatalf("%s reports %s, want %s", remote, got, tag)
			}
		}
	})

	s.run(t, "vm_state", func(t *testing.T, ev *Evidence) {
		st, _, err := s.vm.Status(s.ctx)
		must(t, err, "limactl list")
		ev.Note("VM %s status %s; keep=%v", s.cfg.VM, st, s.cfg.KeepVM)
		if !s.cfg.KeepVM {
			must(t, s.vm.Delete(s.ctx), "deleting the VM")
			ev.Note("VM deleted (set KRILL_ACCEPT_KEEP_VM=1 to keep it)")
		}
	})
}

// ---------------------------------------------------------------------------------
// Phase B: the full scenario chain. Publishes throwaway releases to the test
// repository and steers its "latest" release.
// ---------------------------------------------------------------------------------

func TestSelfUpdateAcceptance(t *testing.T) {
	s := newSuite(t)

	s.run(t, "build_releases", s.buildReleases)

	s.run(t, "publish_releases", func(t *testing.T, ev *Evidence) {
		for _, v := range Variants {
			must(t, PublishRelease(s.ctx, t, s.cfg, s.rels[v.Tag]), "publishing "+v.Tag)
			ev.Note("published %s to %s", v.Tag, s.cfg.Repo)
		}
	})

	s.run(t, "vm_up", func(t *testing.T, ev *Evidence) {
		must(t, s.vm.Ensure(s.ctx), "creating/starting the VM")
	})

	// 1.
	s.run(t, "install_v0.90.0", func(t *testing.T, ev *Evidence) {
		s.installFresh(t, ev, s.rels["v0.90.0"])
		s.expectVersion(t, ev, "v0.90.0")
		s.expectSchema(t, ev, 46)
		s.expectUnitActive(t, ev)
	})

	// 2.
	s.run(t, "busy_refused_at_click", func(t *testing.T, ev *Evidence) {
		s.createBusyApp(t, ev)
		// Discover the release first, so the deploy's in-flight window is
		// not spent waiting for GitHub.
		s.latest(t, ev, "v0.90.1")
		s.deploy(t, ev)
		resp, err := s.client.Install(s.ctx, s.orgID, "v0.90.1")
		must(t, err, "install")
		flashContains(t, ev, "POST install v0.90.1 while deploying", resp, "Not now: a deploy is running.")
		if strings.Contains(resp.Text(), "Downloading") {
			t.Fatalf("a job started despite the running deploy")
		}
		s.expectVersion(t, ev, "v0.90.0")
		s.expectNoJobTrace(t, ev)
	})

	// 3.
	s.run(t, "busy_during_download", func(t *testing.T, ev *Evidence) {
		s.waitNoRunningDeploy(t, ev)
		t.Cleanup(func() {
			out, err := s.vm.Sudo(s.ctx, unthrottleScript)
			ev.Output("remove throttle", out)
			if err != nil {
				t.Errorf("removing the throttle: %v", err)
			}
		})
		s.sudo(t, ev, throttleScript)

		resp, err := s.client.Install(s.ctx, s.orgID, "v0.90.1")
		must(t, err, "install")
		flashContains(t, ev, "POST install v0.90.1 (throttled)", resp, "The update has started.")
		s.waitPage(t, ev, 60*time.Second, "Downloading", "Version v0.90.1")
		s.deploy(t, ev)
		start := time.Now()
		s.waitPage(t, ev, 20*time.Minute, "Failed", "The update failed: a deploy is running")
		ev.Note("job failed with the busy reason %s after the deploy started", time.Since(start).Round(time.Second))
		s.expectVersion(t, ev, "v0.90.0")
		s.expectNoJobTrace(t, ev)
		leftovers := s.sudo(t, ev, "ls -la /usr/local/bin/")
		if strings.Contains(leftovers, ".krill-v0.90.1.tmp") {
			t.Fatalf("staged download left behind in /usr/local/bin")
		}
	})

	// 4.
	s.run(t, "update_to_v0.90.1", func(t *testing.T, ev *Evidence) {
		s.waitNoRunningDeploy(t, ev)
		// Quiet the always-unhealthy app; a stopped app is not busy work.
		s.post(t, ev, s.app.path(s.orgID)+"/stop", nil)
		s.waitAvailable(t, ev, "v0.90.1")
		s.updateTo(t, ev, "v0.90.0", "v0.90.1", 47)
	})

	// 5.
	s.run(t, "manual_rollback_to_v0.90.0", func(t *testing.T, ev *Evidence) {
		s.waitPage(t, ev, 30*time.Second, "Roll back to v0.90.0")
		cursor := s.journalCursor(t)
		resp, err := s.client.Rollback(s.ctx, s.orgID)
		must(t, err, "rollback")
		flashContains(t, ev, "POST rollback", resp, "The rollback has started.")
		s.followRestart(t, ev, "v0.90.1", 5*time.Minute)
		s.waitServing(t, ev, 2*time.Minute)
		text := s.waitPage(t, ev, 2*time.Minute, "Running version v0.90.0", "Rolled back from v0.90.1 to v0.90.0.")
		if strings.Contains(text, "Roll back to v0.90.1") {
			t.Fatalf("page offers a rollback to the newer v0.90.1")
		}
		s.expectVersion(t, ev, "v0.90.0")
		s.expectSchema(t, ev, 47)
		prev, ok, err := s.host.PrevVersion(s.ctx)
		must(t, err, "krill.prev")
		ev.Note("krill.prev after the rollback: present=%v version=%s", ok, prev)
		j := s.journal(t, ev, cursor)
		if !strings.Contains(j, "database schema is newer than this binary") {
			t.Fatalf("journal lacks the newer-schema warning")
		}
		s.expectUnitActive(t, ev)
	})

	// 6.
	s.run(t, "update_again_to_v0.90.1", func(t *testing.T, ev *Evidence) {
		s.waitAvailable(t, ev, "v0.90.1")
		s.updateTo(t, ev, "v0.90.0", "v0.90.1", 47)
	})

	// 7.
	s.run(t, "crash_loop_auto_revert", func(t *testing.T, ev *Evidence) {
		s.latest(t, ev, "v0.90.2")
		cursor := s.journalCursor(t)
		resp, err := s.client.Install(s.ctx, s.orgID, "v0.90.2")
		must(t, err, "install")
		flashContains(t, ev, "POST install v0.90.2", resp, "The update has started.")
		clicked := time.Now()
		s.waitInstalled(t, ev, "v0.90.2", 10*time.Minute)

		// Within a minute the unit must be visibly crash-looping, not failed.
		minR, maxR := -1, -1
		for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(3 * time.Second) {
			st, err := s.host.UnitState(s.ctx)
			must(t, err, "unit state")
			if st.ActiveState == "failed" {
				t.Fatalf("unit landed in failed while crash-looping: %s", st.Raw)
			}
			if minR < 0 || st.NRestarts < minR {
				minR = st.NRestarts
			}
			if st.NRestarts > maxR {
				maxR = st.NRestarts
			}
		}
		ev.Note("NRestarts over the first minute: %d -> %d", minR, maxR)
		if maxR <= minR {
			t.Fatalf("NRestarts did not grow (%d -> %d): the unit is not restarting", minR, maxR)
		}

		// The revert timer fires 10 minutes after it was armed.
		deadline := clicked.Add(13 * time.Minute)
		for {
			st, err := s.host.UnitState(s.ctx)
			must(t, err, "unit state")
			if st.ActiveState == "failed" {
				t.Fatalf("unit landed in failed while waiting for the revert: %s", st.Raw)
			}
			v, verr := s.host.KrillVersion(s.ctx)
			code, herr := s.client.Serving(s.ctx)
			if verr == nil && v == "v0.90.1" && st.ActiveState == "active" && herr == nil && code == http.StatusOK {
				ev.Note("reverted to v0.90.1 and serving %s after the click (NRestarts at revert: %d)", time.Since(clicked).Round(time.Second), st.NRestarts)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("not reverted 13 minutes after the click: version %q, unit %s", v, st.Raw)
			}
			time.Sleep(10 * time.Second)
		}
		s.login(t, ev)
		s.waitPage(t, ev, 2*time.Minute, "The update to v0.90.2 did not start and was rolled back")
		s.expectVersion(t, ev, "v0.90.1")
		s.expectSchema(t, ev, 48)
		j := s.journal(t, ev, cursor)
		for _, want := range []string{"acceptance: simulated crash after migrations", "database schema is newer than this binary"} {
			if !strings.Contains(j, want) {
				t.Fatalf("journal lacks %q", want)
			}
		}
		reverted, err := s.host.FileExists(s.ctx, revertedMark)
		must(t, err, "reverted marker")
		ev.Note("%s present: %v", revertedMark, reverted)
		if reverted {
			t.Fatalf("%s was not consumed", revertedMark)
		}
		s.expectUnitActive(t, ev)
	})

	// 8.
	s.run(t, "installer_over_ui_update_and_sigterm_mid_migration", func(t *testing.T, ev *Evidence) {
		cursor := s.journalCursor(t)
		s.runInstaller(t, ev, s.rels["v0.90.3"])

		sql := "select count(*) from pg_stat_activity where query ilike '%pg_sleep(45)%' and pid <> pg_backend_pid()"
		start := time.Now()
		for {
			out, err := s.host.Psql(s.ctx, sql)
			if err == nil && out != "0" && out != "" {
				ev.Note("pg_sleep seen in pg_stat_activity %s after install.sh returned", time.Since(start).Round(100*time.Millisecond))
				break
			}
			if time.Since(start) > 2*time.Minute {
				t.Fatalf("migration 49's pg_sleep never showed up in pg_stat_activity (last %q, err %v)", out, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		stopStart := time.Now()
		s.sudo(t, ev, "systemctl stop krill")
		ev.Note("systemctl stop krill returned after %s", time.Since(stopStart).Round(100*time.Millisecond))
		v, dirty, err := s.host.SchemaVersion(s.ctx)
		must(t, err, "schema")
		ev.Note("schema after the stop: version=%d dirty=%v", v, dirty)
		if dirty || (v != 48 && v != 49) {
			t.Fatalf("schema after SIGTERM mid-migration is %d dirty=%v, want 48 or 49 clean", v, dirty)
		}
		j := s.journal(t, ev, cursor)
		ev.Note("journal mentions 'migrations stopped by shutdown': %v", strings.Contains(j, "migrations stopped by shutdown"))

		s.sudo(t, ev, "systemctl start krill")
		s.waitServing(t, ev, 5*time.Minute)
		s.login(t, ev)
		s.expectVersion(t, ev, "v0.90.3")
		s.expectSchema(t, ev, 49)
		s.expectPrev(t, ev, "")
		drop, err := s.host.DropInPresent(s.ctx)
		must(t, err, "drop-in")
		if !drop {
			t.Fatalf("install.sh removed the update drop-in")
		}
		text := s.page(t, ev)
		if strings.Contains(text, "Roll back to") {
			t.Fatalf("page offers a rollback after install.sh removed krill.prev")
		}
		s.expectUnitActive(t, ev)
	})

	// 9.
	s.run(t, "failing_migration_documented_recovery", func(t *testing.T, ev *Evidence) {
		// The recovery below follows docs/install.md; fail first if the
		// document no longer says what is executed.
		doc, err := os.ReadFile(filepath.Join(s.rels["v0.90.3"].Source, "docs", "install.md"))
		must(t, err, "reading docs/install.md")
		for _, want := range []string{
			"database schema is dirty at version N",
			"docker exec krill-postgres pg_dump -U krill krill > krill-backup.sql",
			"docker exec -it krill-postgres psql -U krill krill",
			"UPDATE schema_migrations SET version = <N-1>, dirty = false;",
			"5. `systemctl restart krill`.",
		} {
			if !strings.Contains(string(doc), want) {
				t.Fatalf("docs/install.md Rollback no longer contains %q; the documented recovery changed", want)
			}
		}

		s.latest(t, ev, "v0.90.4")
		cursor := s.journalCursor(t)
		resp, err := s.client.Install(s.ctx, s.orgID, "v0.90.4")
		must(t, err, "install")
		flashContains(t, ev, "POST install v0.90.4", resp, "The update has started.")
		clicked := time.Now()
		s.waitInstalled(t, ev, "v0.90.4", 10*time.Minute)

		// The timer puts v0.90.3 back, and it refuses the dirty schema too.
		deadline := clicked.Add(13 * time.Minute)
		for {
			v, _ := s.host.KrillVersion(s.ctx)
			if v == "v0.90.3" {
				j, err := s.host.JournalSince(s.ctx, cursor)
				must(t, err, "journal")
				i := strings.LastIndex(j, "version=v0.90.3")
				if i >= 0 && strings.Contains(j[i:], "database schema is dirty at version 50") {
					ev.Note("v0.90.3 back and refusing the dirty schema %s after the click", time.Since(clicked).Round(time.Second))
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("v0.90.3 not back and refusing the dirty schema 13 minutes after the click (binary %q)", v)
			}
			time.Sleep(10 * time.Second)
		}
		j := s.journal(t, ev, cursor)
		v, dirty, err := s.host.SchemaVersion(s.ctx)
		must(t, err, "schema")
		ev.Note("schema: version=%d dirty=%v", v, dirty)
		if v != 50 || !dirty {
			t.Fatalf("schema is %d dirty=%v, want 50 dirty", v, dirty)
		}
		st, err := s.host.UnitState(s.ctx)
		must(t, err, "unit state")
		ev.Output("systemctl show krill (before recovery)", st.Raw)

		// docs/install.md, "A failed migration is not rolled back", literally:
		// 1. Back up.
		s.sudo(t, ev, "cd /root && docker exec krill-postgres pg_dump -U krill krill > krill-backup.sql && ls -la /root/krill-backup.sql && head -c 300 /root/krill-backup.sql")
		// 2. Which migration failed and why.
		for _, line := range strings.Split(j, "\n") {
			if strings.Contains(line, "acceptance_does_not_exist") || strings.Contains(line, "dirty at version") {
				ev.Note("journal: %s", line)
				break
			}
		}
		// 3. Migration 50 ran in a transaction, so nothing of it is applied:
		// the schema matches version 49 as it is.
		out, err := s.host.Psql(s.ctx, "select to_regclass('acceptance_c') is not null, to_regclass('acceptance_does_not_exist') is null")
		must(t, err, "verifying the schema matches 49")
		ev.Output("schema matches 49 (acceptance_c exists, migration 50 left nothing)", out)
		if out != "t|t" {
			t.Fatalf("schema does not match version 49: %q", out)
		}
		// 4. Mark the version clean so the binary without migration 50
		// starts. The doc's `docker exec -it` needs a terminal; the
		// harness has none, so the same psql session gets its SQL on stdin
		// (-i, without -t).
		s.sudo(t, ev, `echo 'UPDATE schema_migrations SET version = 49, dirty = false;' | docker exec -i krill-postgres psql -U krill krill`)
		// 5. Restart (after clearing a failed state, as the manual rollback
		// command in the same section does).
		s.sudo(t, ev, "systemctl reset-failed krill && systemctl restart krill")

		s.waitServing(t, ev, 5*time.Minute)
		s.login(t, ev)
		s.expectVersion(t, ev, "v0.90.3")
		s.expectSchema(t, ev, 49)
		s.expectUnitActive(t, ev)
		text := s.page(t, ev)
		ev.Note("page reports the revert: %v", strings.Contains(text, "The update to v0.90.4 did not start and was rolled back"))
	})

	// 10.
	s.run(t, "cleanup", func(t *testing.T, ev *Evidence) {
		if s.cfg.KeepVM {
			ev.Note("KRILL_ACCEPT_KEEP_VM=1: keeping VM %s", s.cfg.VM)
			return
		}
		must(t, s.vm.Delete(s.ctx), "deleting the VM")
		ev.Note("deleted VM %s", s.cfg.VM)
	})
}
