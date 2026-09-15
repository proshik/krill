package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testRepo    = "owner/repo"
	testTag     = "v0.2.0"
	testCurrent = "v0.1.0"
	testUnit    = "krill.service"
	oldContent  = "old-binary"
	prevContent = "prev-binary"
)

const stopTimerCmd = "systemctl stop krill-update-revert.timer krill-update-restart.timer 2>/dev/null; " +
	"systemctl reset-failed krill-update-revert.timer krill-update-revert.service " +
	"krill-update-restart.timer krill-update-restart.service 2>/dev/null; true"

const (
	daemonReloadCmd = "systemctl daemon-reload"
	restartCmd      = "systemd-run --on-active=2 --timer-property=AccuracySec=100ms --unit=krill-update-restart systemctl restart 'krill.service'"
)

var testNow = time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)

// runCall is one command the fake runner received, with a snapshot of the
// install state at that moment.
type runCall struct {
	cmd         string
	binOriginal bool // bin is still the inode it was before the job
	pending     bool // the pending marker exists
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []runCall
	bin     string
	orig    os.FileInfo
	pending string
	// fail, when set, makes a command fail; n counts earlier calls of the
	// same command.
	fail func(cmd string, n int) bool
	// onRun, when set, runs after the snapshot is taken — a way to change
	// the filesystem at a precise step of the job.
	onRun func(cmd string)
}

func (f *fakeRunner) Run(_ context.Context, _, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := runCall{cmd: cmd}
	if fi, err := os.Stat(f.bin); err == nil {
		call.binOriginal = os.SameFile(fi, f.orig)
	}
	if _, err := os.Stat(f.pending); err == nil {
		call.pending = true
	}
	n := 0
	for _, c := range f.calls {
		if c.cmd == cmd {
			n++
		}
	}
	f.calls = append(f.calls, call)
	if f.onRun != nil {
		f.onRun(cmd)
	}
	if f.fail != nil && f.fail(cmd, n) {
		return "boom\n", errors.New("exit status 1")
	}
	return "", nil
}

func (f *fakeRunner) snapshot() []runCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runCall(nil), f.calls...)
}

func (f *fakeRunner) cmds() []string {
	var out []string
	for _, c := range f.snapshot() {
		out = append(out, c.cmd)
	}
	return out
}

// harness is an Updater wired to temp dirs, an httptest "GitHub", a fake
// runner and a fake smoke test.
type harness struct {
	u       *Updater
	runner  *fakeRunner
	checker *Checker

	binDir, runDir, unitDir string
	bin, prev, pending      string
	origBin                 os.FileInfo

	mu            sync.Mutex
	asset         []byte
	checksums     string
	contentLength string        // overrides the asset's Content-Length header when set
	block         chan struct{} // when set, the checksums handler waits for it to close
	stagedOut     string        // what the staged binary prints for --version
	prevOut       string        // what <bin>.prev prints for --version
	execCalls     int
	stagedSeen    []byte      // content of the staged binary when it was smoke-tested
	stagedMode    os.FileMode // its mode at that moment
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		binDir:    filepath.Join(root, "bin"),
		runDir:    filepath.Join(root, "run"),
		unitDir:   filepath.Join(root, "systemd"),
		asset:     []byte("new-binary-" + testTag),
		stagedOut: "krill " + testTag + "\n",
	}
	h.checksums = sha256Hex(h.asset) + "  krill-linux-amd64\n" + hashB + "  krill-linux-arm64\n"
	for _, d := range []string{h.binDir, h.runDir, h.unitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.bin = filepath.Join(h.binDir, "krill")
	h.prev = h.bin + ".prev"
	h.pending = filepath.Join(h.runDir, pendingMarkerName)
	if err := os.WriteFile(h.bin, []byte(oldContent), 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.origBin, err = os.Stat(h.bin); err != nil {
		t.Fatal(err)
	}

	// Like GitHub, the release download URLs redirect to object storage, so
	// a client that stops following redirects fails every install.
	prefix := "/" + testRepo + "/releases/download/" + testTag + "/"
	mux := http.NewServeMux()
	for _, name := range []string{"checksums.txt", "krill-linux-amd64"} {
		mux.Handle(prefix+name, http.RedirectHandler("/objects/"+name+"?signature=abc", http.StatusFound))
	}
	mux.HandleFunc("/objects/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		block, body := h.block, h.checksums
		h.mu.Unlock()
		if block != nil {
			<-block
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/objects/krill-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		body, cl := h.asset, h.contentLength
		h.mu.Unlock()
		if cl == "" {
			cl = strconv.Itoa(len(body))
		}
		w.Header().Set("Content-Length", cl)
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	h.checker = NewChecker(testRepo, 0, true)
	h.checker.current = testCurrent
	h.checker.state.Latest = Release{Tag: testTag, URL: srv.URL + "/" + testRepo + "/releases/tag/" + testTag}

	h.runner = &fakeRunner{bin: h.bin, orig: h.origBin, pending: h.pending}
	u := NewUpdater(h.runner, h.checker, testRepo, true, nil)
	u.baseURL = srv.URL
	u.current = testCurrent
	u.binPath = h.bin
	u.runDir = h.runDir
	u.unitDir = h.unitDir
	u.unit = testUnit
	u.goos = "linux"
	u.goarch = "amd64"
	u.getenv = func(k string) string {
		if k == "INVOCATION_ID" {
			return "0123abcd"
		}
		return ""
	}
	u.getppid = func() int { return 1 }
	u.geteuid = func() int { return 0 }
	u.freeBytes = func(string) (uint64, error) { return 1 << 40, nil }
	u.execVersion = h.execVersion
	u.now = func() time.Time { return testNow }
	u.stopBackoff = 0
	h.u = u
	t.Cleanup(h.u.wg.Wait)
	return h
}

func (h *harness) execVersion(_ context.Context, path string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.execCalls++
	switch {
	case path == h.prev:
		return h.prevOut, nil
	case filepath.Dir(path) == h.binDir && strings.HasPrefix(filepath.Base(path), ".krill-"):
		h.stagedSeen, _ = os.ReadFile(path)
		if fi, err := os.Stat(path); err == nil {
			h.stagedMode = fi.Mode().Perm()
		}
		return h.stagedOut, nil
	}
	return "", errors.New("unexpected exec of " + path)
}

func (h *harness) set(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn()
}

func (h *harness) execCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.execCalls
}

func (h *harness) systemdRunCmd() string {
	return "systemd-run --on-active=600 --timer-property=AccuracySec=1s --unit=krill-update-revert sh -c " +
		shellQuote(revertScript(h.bin, h.runDir, testUnit, testTag))
}

// startAndWait starts an update of testTag and waits for the job to end.
func (h *harness) startAndWait(t *testing.T) Status {
	t.Helper()
	if err := h.u.Start(testTag); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	h.u.wg.Wait()
	return h.u.Status()
}

func assertCmds(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("runner commands =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), data, want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists (stat err %v), want it missing", path, err)
	}
}

func assertSameFile(t *testing.T, path string, want os.FileInfo) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !os.SameFile(fi, want) {
		t.Errorf("%s is not the expected inode", filepath.Base(path))
	}
}

func assertNoStaged(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".krill-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("staged files left behind: %v", matches)
	}
}

func assertFailed(t *testing.T, st Status, errContains string) {
	t.Helper()
	if st.Phase != PhaseFailed || st.Err == nil {
		t.Fatalf("Status() = %+v, want phase failed with an error", st)
	}
	if !strings.Contains(st.Err.Error(), errContains) {
		t.Errorf("Status().Err = %q, want it to mention %q", st.Err, errContains)
	}
}

// assertUntouched checks a job that failed before the swap left the
// installation exactly as it was and never restarted the unit.
func (h *harness) assertUntouched(t *testing.T) {
	t.Helper()
	assertSameFile(t, h.bin, h.origBin)
	assertContent(t, h.bin, oldContent)
	assertMissing(t, h.prev)
	assertMissing(t, h.pending)
	assertNoStaged(t, h.binDir)
	for _, c := range h.runner.cmds() {
		if c == restartCmd {
			t.Errorf("unexpected command %q", c)
		}
	}
}

// armRevertPrefix starts the command that arms the revert timer (the
// restart request is a systemd-run too, with a different unit).
const armRevertPrefix = "systemd-run --on-active=600 "

// assertNoTimer checks the revert timer was never armed.
func (h *harness) assertNoTimer(t *testing.T) {
	t.Helper()
	for _, c := range h.runner.cmds() {
		if strings.HasPrefix(c, armRevertPrefix) || c == restartCmd {
			t.Errorf("unexpected command %q", c)
		}
	}
}

type pendingJSON struct {
	From      string `json:"from"`
	To        string `json:"to"`
	StartedAt string `json:"started_at"`
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v (%q)", path, err, data)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- Start: the install job -------------------------------------------------

func TestStart_Success(t *testing.T) {
	h := newHarness(t)
	st := h.startAndWait(t)

	if st.Phase != PhaseRestarting || st.Err != nil || st.Tag != testTag || !st.StartedAt.Equal(testNow) {
		t.Fatalf("Status() = %+v, want restarting %s since %v with no error", st, testTag, testNow)
	}
	calls := h.runner.snapshot()
	assertCmds(t, h.runner.cmds(), stopTimerCmd, daemonReloadCmd, h.systemdRunCmd(), restartCmd)
	if len(calls) == 4 {
		if run := calls[2]; !run.binOriginal || run.pending {
			t.Errorf("at systemd-run: binOriginal=%v pending=%v, want the old binary and no marker", run.binOriginal, run.pending)
		}
		if rs := calls[3]; rs.binOriginal || !rs.pending {
			t.Errorf("at restart: binOriginal=%v pending=%v, want the new binary and the marker", rs.binOriginal, rs.pending)
		}
	}

	assertContent(t, h.bin, string(h.asset))
	fi, err := os.Stat(h.bin)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("bin mode = %v, want 0755", fi.Mode().Perm())
	}
	assertSameFile(t, h.prev, h.origBin)
	assertContent(t, h.prev, oldContent)
	assertNoStaged(t, h.binDir)
	assertContent(t, filepath.Join(h.unitDir, "krill.service.d", "10-krill-update.conf"), "[Unit]\nStartLimitIntervalSec=0\n")

	var m pendingJSON
	readJSON(t, h.pending, &m)
	if want := (pendingJSON{From: testCurrent, To: testTag, StartedAt: "2026-09-15T12:30:00Z"}); m != want {
		t.Errorf("pending marker = %+v, want %+v", m, want)
	}
	if pfi, err := os.Stat(h.pending); err == nil && pfi.Mode().Perm() != 0o600 {
		t.Errorf("pending marker mode = %v, want 0600", pfi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(h.runDir); len(entries) != 1 {
		t.Errorf("run dir holds %d entries, want only the pending marker", len(entries))
	}

	h.mu.Lock()
	seen, mode := h.stagedSeen, h.stagedMode
	h.mu.Unlock()
	if !bytes.Equal(seen, h.asset) || mode != 0o755 {
		t.Errorf("smoke test saw content %q mode %v, want the downloaded asset with mode 0755", seen, mode)
	}

	// The job is over but the process is about to restart: no second job.
	if err := h.u.Start(testTag); !errors.Is(err, ErrJobRunning) {
		t.Errorf("Start() after a successful install = %v, want ErrJobRunning", err)
	}
}

func TestStart_DropInAlreadyPresent(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.unitDir, "krill.service.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "10-krill-update.conf"), "[Unit]\nStartLimitIntervalSec=0\n")

	st := h.startAndWait(t)
	if st.Phase != PhaseRestarting {
		t.Fatalf("Status() = %+v, want restarting", st)
	}
	assertCmds(t, h.runner.cmds(), stopTimerCmd, h.systemdRunCmd(), restartCmd)
}

func TestStart_ReplacesAnOlderPrevious(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.prev, prevContent)
	st := h.startAndWait(t)
	if st.Phase != PhaseRestarting {
		t.Fatalf("Status() = %+v, want restarting", st)
	}
	assertSameFile(t, h.prev, h.origBin)
}

func TestStart_FailsBeforeTheTimer(t *testing.T) {
	// wantExec is how many times the staged binary may be run: never, unless
	// it downloaded completely and matched its checksum.
	cases := []struct {
		name        string
		setup       func(h *harness)
		errContains string
		wantExec    int
	}{
		{"checksums without the asset", func(h *harness) {
			h.checksums = hashB + "  krill-linux-arm64\n"
		}, "krill-linux-amd64", 0},
		{"checksums malformed", func(h *harness) {
			h.checksums = "this is not a checksum listing\n"
		}, "checksums.txt", 0},
		{"hash mismatch", func(h *harness) {
			h.asset = []byte("tampered")
		}, "checksum mismatch", 0},
		{"smoke test prints another version", func(h *harness) {
			h.stagedOut = "krill v0.1.9\n"
		}, "krill v0.1.9", 1},
		{"not enough free space", func(h *harness) {
			h.u.freeBytes = func(string) (uint64, error) { return 1 << 20, nil }
		}, "free space", 0},
		{"asset larger than the cap", func(h *harness) {
			h.contentLength = strconv.Itoa(300 << 20)
		}, "too large", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.set(func() { tc.setup(h) })
			st := h.startAndWait(t)
			assertFailed(t, st, tc.errContains)
			h.assertUntouched(t)
			h.assertNoTimer(t)
			if n := h.execCount(); n != tc.wantExec {
				t.Errorf("staged binary executed %d times, want %d", n, tc.wantExec)
			}
		})
	}
}

func TestStart_FailsOnHTTPErrors(t *testing.T) {
	h := newHarness(t)
	h.u.repo = "owner/missing"
	st := h.startAndWait(t)
	assertFailed(t, st, "404")
	h.assertUntouched(t)
	h.assertNoTimer(t)
	if n := h.execCount(); n != 0 {
		t.Errorf("staged binary executed %d times, want 0", n)
	}
}

func TestStart_SystemdRunFails(t *testing.T) {
	h := newHarness(t)
	h.runner.fail = func(cmd string, _ int) bool { return strings.HasPrefix(cmd, armRevertPrefix) }
	st := h.startAndWait(t)
	assertFailed(t, st, "revert timer")
	h.assertUntouched(t)
	cmds := h.runner.cmds()
	if len(cmds) < 3 || cmds[2] != h.systemdRunCmd() {
		t.Errorf("runner commands = %q, want systemd-run as the third", cmds)
	}
}

func TestStart_RestartFails(t *testing.T) {
	h := newHarness(t)
	h.runner.fail = func(cmd string, _ int) bool { return cmd == restartCmd }
	st := h.startAndWait(t)
	assertFailed(t, st, "restart")

	assertSameFile(t, h.bin, h.origBin)
	assertContent(t, h.bin, oldContent)
	assertMissing(t, h.prev)
	assertMissing(t, h.pending)
	assertNoStaged(t, h.binDir)
	assertCmds(t, h.runner.cmds(), stopTimerCmd, daemonReloadCmd, h.systemdRunCmd(), restartCmd, stopTimerCmd)
}

// The staged binary vanishing after the timer is armed makes the rename
// fail after .prev was linked: nothing may change, and the timer and the
// marker must go.
func TestStart_RenameFails(t *testing.T) {
	h := newHarness(t)
	h.runner.onRun = func(cmd string) {
		if strings.HasPrefix(cmd, "systemd-run") {
			_ = os.Remove(filepath.Join(h.binDir, ".krill-"+testTag+".tmp"))
		}
	}
	st := h.startAndWait(t)
	assertFailed(t, st, "installing the new binary")
	h.assertUntouched(t)
	assertCmds(t, h.runner.cmds(), stopTimerCmd, daemonReloadCmd, h.systemdRunCmd(), stopTimerCmd)
}

// When the old binary cannot be put back after a failed restart, the revert
// timer is the only thing left that can: it stays armed, with its marker.
func TestStart_RestoreFailsLeavesTheTimerArmed(t *testing.T) {
	h := newHarness(t)
	h.runner.onRun = func(cmd string) {
		if cmd == restartCmd {
			_ = os.Remove(h.prev)
		}
	}
	h.runner.fail = func(cmd string, _ int) bool { return cmd == restartCmd }
	st := h.startAndWait(t)
	assertFailed(t, st, "restart")
	assertCmds(t, h.runner.cmds(), stopTimerCmd, daemonReloadCmd, h.systemdRunCmd(), restartCmd)
	if _, err := os.Stat(h.pending); err != nil {
		t.Errorf("pending marker removed (%v), want it kept for the revert timer", err)
	}
	if err := h.u.Start(testTag); !errors.Is(err, ErrPending) {
		t.Errorf("Start() = %v, want ErrPending while the revert is outstanding", err)
	}
}

func TestStart_RetryAfterFailure(t *testing.T) {
	h := newHarness(t)
	h.set(func() { h.stagedOut = "garbage" })
	assertFailed(t, h.startAndWait(t), "garbage")

	h.set(func() { h.stagedOut = "krill " + testTag + "\n" })
	if st := h.startAndWait(t); st.Phase != PhaseRestarting {
		t.Fatalf("Status() after retry = %+v, want restarting", st)
	}
}

func TestStart_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		tag   string
		setup func(t *testing.T, h *harness)
		check func(t *testing.T, err error)
	}{
		{"unsupported", testTag, func(_ *testing.T, h *harness) { h.u.goos = "darwin" }, func(t *testing.T, err error) {
			if !errors.Is(err, ErrUnsupported) || !strings.HasSuffix(err.Error(), ": os") {
				t.Errorf("err = %v, want ErrUnsupported with reason os", err)
			}
		}},
		{"invalid tag", "v0.2", nil, wantIs(ErrInvalidTag)},
		{"invalid tag with shell metacharacters", "v0.2.0;reboot", nil, wantIs(ErrInvalidTag)},
		{"not the cached latest", "v0.3.0", nil, wantIs(ErrNotLatest)},
		{"nothing discovered yet", testTag, func(_ *testing.T, h *harness) { h.checker.state.Latest = Release{} }, wantIs(ErrNotLatest)},
		{"not newer", testCurrent, func(_ *testing.T, h *harness) { h.checker.state.Latest = Release{Tag: testCurrent} }, wantIs(ErrNotNewer)},
		{"dev build", testTag, func(_ *testing.T, h *harness) { h.u.current = "dev+abc123" }, wantIs(ErrNotNewer)},
		{"pending marker", testTag, func(t *testing.T, h *harness) { writeFile(t, h.pending, "{}") }, wantIs(ErrPending)},
		{"busy", testTag, func(_ *testing.T, h *harness) {
			h.u.busy = func(context.Context) string { return "deploy" }
		}, func(t *testing.T, err error) {
			var be *BusyError
			if !errors.As(err, &be) || be.Reason != "deploy" {
				t.Errorf("err = %v, want BusyError{deploy}", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(t, h)
			}
			tc.check(t, h.u.Start(tc.tag))
			h.u.wg.Wait()
			if st := h.u.Status(); st.Phase != PhaseIdle {
				t.Errorf("Status() = %+v, want idle", st)
			}
			assertCmds(t, h.runner.cmds())
			assertSameFile(t, h.bin, h.origBin)
		})
	}
}

func wantIs(target error) func(t *testing.T, err error) {
	return func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, target) {
			t.Errorf("err = %v, want %v", err, target)
		}
	}
}

func TestStart_RefusalReleasesTheJobSlot(t *testing.T) {
	h := newHarness(t)
	h.u.busy = func(context.Context) string { return "backup" }
	var be *BusyError
	if err := h.u.Start(testTag); !errors.As(err, &be) {
		t.Fatalf("Start() = %v, want BusyError", err)
	}
	h.u.busy = func(context.Context) string { return "" }
	if st := h.startAndWait(t); st.Phase != PhaseRestarting {
		t.Fatalf("Status() = %+v, want restarting", st)
	}
}

func TestStart_JobRunning(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	h.set(func() { h.block = block })

	if err := h.u.Start(testTag); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if st := h.u.Status(); st.Phase != PhaseDownloading || st.Tag != testTag {
		t.Errorf("Status() = %+v, want downloading %s", st, testTag)
	}
	if err := h.u.Start(testTag); !errors.Is(err, ErrJobRunning) {
		t.Errorf("second Start() = %v, want ErrJobRunning", err)
	}
	writeFile(t, h.prev, prevContent)
	if err := h.u.Rollback(); !errors.Is(err, ErrJobRunning) {
		t.Errorf("Rollback() during a job = %v, want ErrJobRunning", err)
	}
	close(block)
	h.u.wg.Wait()
	if st := h.u.Status(); st.Phase != PhaseRestarting {
		t.Errorf("Status() = %+v, want restarting", st)
	}
}

func TestStart_Concurrent(t *testing.T) {
	for i := 0; i < 10; i++ {
		h := newHarness(t)
		block := make(chan struct{})
		h.set(func() { h.block = block })

		var wg sync.WaitGroup
		errs := make([]error, 2)
		gate := make(chan struct{})
		for j := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				errs[j] = h.u.Start(testTag)
			}()
		}
		close(gate)
		wg.Wait()
		close(block)
		h.u.wg.Wait()

		ok, running := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrJobRunning):
				running++
			default:
				t.Errorf("Start() = %v", err)
			}
		}
		if ok != 1 || running != 1 {
			t.Fatalf("iteration %d: %d successes and %d ErrJobRunning, want one each", i, ok, running)
		}
		if got := strings.Count(strings.Join(h.runner.cmds(), "\n"), armRevertPrefix); got != 1 {
			t.Fatalf("iteration %d: the revert timer was armed %d times, want 1", i, got)
		}
	}
}

// --- ConfirmStartup -----------------------------------------------------------

func writePending(t *testing.T, h *harness, from, to string) {
	t.Helper()
	data, err := json.Marshal(pendingJSON{From: from, To: to, StartedAt: "2026-09-15T12:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, h.pending, string(data))
}

func assertLast(t *testing.T, u *Updater, want Result) {
	t.Helper()
	got, ok := u.LastResult()
	if !ok || got != want {
		t.Errorf("LastResult() = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestConfirmStartup_Confirms(t *testing.T) {
	h := newHarness(t)
	h.u.current = testTag
	writePending(t, h, testCurrent, testTag)

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, h.pending)
	calls := h.runner.snapshot()
	assertCmds(t, h.runner.cmds(), stopTimerCmd)
	if len(calls) == 1 && calls[0].pending {
		t.Error("the timer was stopped while the pending marker still existed; the marker must go first")
	}
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
}

func TestConfirmStartup_RetriesStoppingTheTimer(t *testing.T) {
	t.Run("third attempt succeeds", func(t *testing.T) {
		h := newHarness(t)
		h.u.current = testTag
		writePending(t, h, testCurrent, testTag)
		h.runner.fail = func(_ string, n int) bool { return n < 2 }
		h.u.ConfirmStartup(context.Background())
		assertCmds(t, h.runner.cmds(), stopTimerCmd, stopTimerCmd, stopTimerCmd)
		assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
	})
	t.Run("every attempt fails", func(t *testing.T) {
		h := newHarness(t)
		h.u.current = testTag
		writePending(t, h, testCurrent, testTag)
		h.runner.fail = func(string, int) bool { return true }
		h.u.ConfirmStartup(context.Background())
		assertCmds(t, h.runner.cmds(), stopTimerCmd, stopTimerCmd, stopTimerCmd)
		assertMissing(t, h.pending)
		assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
	})
}

func TestConfirmStartup_Interrupted(t *testing.T) {
	h := newHarness(t)
	writePending(t, h, testCurrent, testTag) // we still run testCurrent

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, h.pending)
	assertCmds(t, h.runner.cmds(), stopTimerCmd)
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: false, Reason: "interrupted"})
}

// The previous process died between linking .prev and renaming the new
// binary into place: .prev is just another name for the running binary and
// must not be offered as a rollback target.
func TestConfirmStartup_InterruptedRemovesLinkedPrev(t *testing.T) {
	h := newHarness(t)
	writePending(t, h, testCurrent, testTag)
	if err := os.Link(h.bin, h.prev); err != nil {
		t.Fatal(err)
	}

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, h.prev)
	assertSameFile(t, h.bin, h.origBin)
	if v, ok := h.u.Previous(context.Background()); ok {
		t.Errorf("Previous() = %q, want none", v)
	}
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: false, Reason: "interrupted"})
}

func TestConfirmStartup_InterruptedKeepsARealPrev(t *testing.T) {
	h := newHarness(t)
	writePending(t, h, testCurrent, testTag)
	writeFile(t, h.prev, prevContent)

	h.u.ConfirmStartup(context.Background())

	assertContent(t, h.prev, prevContent)
}

// A Start or Rollback arriving while ConfirmStartup is still settling the
// previous update must wait its turn.
func TestConfirmStartup_ExcludesJobs(t *testing.T) {
	h := newHarness(t)
	h.u.current = testTag
	writePending(t, h, testCurrent, testTag)
	writeFile(t, h.prev, prevContent)
	h.set(func() { h.prevOut = "krill v0.0.9\n" })
	// Make a later Start otherwise valid: newer latest, no marker left.
	h.checker.state.Latest = Release{Tag: "v0.3.0"}

	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.runner.onRun = func(string) {
		once.Do(func() {
			close(entered)
			<-unblock
		})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.u.ConfirmStartup(context.Background())
	}()
	<-entered

	if err := h.u.Start("v0.3.0"); !errors.Is(err, ErrJobRunning) {
		t.Errorf("Start() during ConfirmStartup = %v, want ErrJobRunning", err)
	}
	if err := h.u.Rollback(); !errors.Is(err, ErrJobRunning) {
		t.Errorf("Rollback() during ConfirmStartup = %v, want ErrJobRunning", err)
	}
	close(unblock)
	<-done
	h.runner.onRun = nil

	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
	// Released afterwards: the next refusal is about the request, not the slot.
	if err := h.u.Start(testTag); !errors.Is(err, ErrNotLatest) {
		t.Errorf("Start() after ConfirmStartup = %v, want ErrNotLatest", err)
	}
}

// A request that holds the job slot while a pending marker exists is only
// validating and about to be refused; confirmation waits for it rather than
// skipping and leaving the revert timer to undo a good update.
func TestConfirmStartup_WaitsForTheSlot(t *testing.T) {
	h := newHarness(t)
	h.u.current = testTag
	h.u.confirmPoll = time.Millisecond
	writePending(t, h, testCurrent, testTag)
	if !h.u.claim() {
		t.Fatal("claim() = false on a fresh updater")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.u.ConfirmStartup(context.Background())
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("ConfirmStartup returned while the slot was still held")
	default:
	}
	if _, err := os.Stat(h.pending); err != nil {
		t.Errorf("pending marker gone before the slot was released (%v)", err)
	}
	h.u.release()
	<-done

	assertMissing(t, h.pending)
	assertCmds(t, h.runner.cmds(), stopTimerCmd)
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
	// ConfirmStartup gave the slot back.
	if !h.u.claim() {
		t.Error("the job slot is still held after ConfirmStartup")
	}
}

// If the slot never frees up, confirming without it beats letting the
// revert timer fire.
func TestConfirmStartup_ProceedsWhenTheWaitExpires(t *testing.T) {
	h := newHarness(t)
	h.u.current = testTag
	h.u.confirmPoll = time.Millisecond
	h.u.confirmWait = 20 * time.Millisecond
	writePending(t, h, testCurrent, testTag)
	if !h.u.claim() {
		t.Fatal("claim() = false on a fresh updater")
	}

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, h.pending)
	assertCmds(t, h.runner.cmds(), stopTimerCmd)
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
	// It must not release a slot it never took.
	if h.u.claim() {
		t.Error("ConfirmStartup released a job slot it did not hold")
	}
	h.u.release()
}

// Confirmation writes nothing to the unit directory, so a directory that
// became unwritable must not stop it from cancelling the revert.
func TestConfirmStartup_UnitDirNotWritable(t *testing.T) {
	h := newHarness(t)
	h.u.current = testTag
	h.u.unitDir = filepath.Join(h.unitDir, "missing")
	writePending(t, h, testCurrent, testTag)
	if ok, reason := h.u.Supported(); ok || reason != "unit_dir_readonly" {
		t.Fatalf("Supported() = (%v, %q), want unit_dir_readonly", ok, reason)
	}

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, h.pending)
	assertCmds(t, h.runner.cmds(), stopTimerCmd)
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
}

// Without a binary path there is no directory to sweep — in particular not
// the working directory filepath.Dir("") would name — but the marker is
// still settled.
func TestConfirmStartup_NoBinaryPath(t *testing.T) {
	h := newHarness(t)
	h.u.current = testTag
	h.u.binPath = ""
	writePending(t, h, testCurrent, testTag)
	cwd := t.TempDir()
	t.Chdir(cwd)
	stray := filepath.Join(cwd, ".krill-x.tmp")
	writeFile(t, stray, "not ours")

	h.u.ConfirmStartup(context.Background())

	assertContent(t, stray, "not ours")
	assertMissing(t, h.pending)
	assertCmds(t, h.runner.cmds(), stopTimerCmd)
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: true})
}

func TestConfirmStartup_Reverted(t *testing.T) {
	h := newHarness(t)
	reverted := filepath.Join(h.runDir, revertedMarkerName)
	writeFile(t, reverted, testTag+"\n")

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, reverted)
	assertCmds(t, h.runner.cmds())
	assertLast(t, h.u, Result{From: testCurrent, To: testTag, OK: false, Reason: "reverted"})
}

func TestConfirmStartup_RevertedWithGarbage(t *testing.T) {
	h := newHarness(t)
	reverted := filepath.Join(h.runDir, revertedMarkerName)
	writeFile(t, reverted, "/bin/sh; rm -rf /\n")

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, reverted)
	assertLast(t, h.u, Result{From: testCurrent, To: "", OK: false, Reason: "reverted"})
}

func TestConfirmStartup_RolledBack(t *testing.T) {
	h := newHarness(t)
	rolledback := filepath.Join(h.runDir, rolledbackMarkerName)
	writeFile(t, rolledback, `{"from":"v0.2.0","to":"v0.1.0"}`)

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, rolledback)
	assertCmds(t, h.runner.cmds())
	assertLast(t, h.u, Result{From: testTag, To: testCurrent, OK: true, Reason: "rollback"})
}

func TestConfirmStartup_NothingToDo(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{".krill-x.tmp", ".krill-v0.2.0.tmp", ".krill-rollback.tmp", ".krill-probe-123.tmp", ".krill-copy-9.tmp", "other.tmp", ".krill-keep"} {
		writeFile(t, filepath.Join(h.binDir, name), "x")
	}

	h.u.ConfirmStartup(context.Background())

	assertCmds(t, h.runner.cmds())
	if got, ok := h.u.LastResult(); ok {
		t.Errorf("LastResult() = %+v, want none", got)
	}
	assertNoStaged(t, h.binDir)
	for _, keep := range []string{"krill", "other.tmp", ".krill-keep"} {
		if _, err := os.Stat(filepath.Join(h.binDir, keep)); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
}

func TestConfirmStartup_CorruptMarker(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.pending, "not json")

	h.u.ConfirmStartup(context.Background())

	assertMissing(t, h.pending)
	assertCmds(t, h.runner.cmds(), stopTimerCmd)
	if got, ok := h.u.LastResult(); ok {
		t.Errorf("LastResult() = %+v, want none", got)
	}
}

func TestConfirmStartup_Unsupported(t *testing.T) {
	h := newHarness(t)
	h.u.getppid = func() int { return 4242 }
	writePending(t, h, testCurrent, testTag)
	stray := filepath.Join(h.binDir, ".krill-x.tmp")
	writeFile(t, stray, "x")

	h.u.ConfirmStartup(context.Background())

	assertCmds(t, h.runner.cmds())
	assertContent(t, h.pending, `{"from":"v0.1.0","to":"v0.2.0","started_at":"2026-09-15T12:00:00Z"}`)
	assertContent(t, stray, "x")
	if _, ok := h.u.LastResult(); ok {
		t.Error("LastResult() set on an unsupported host")
	}
}

// --- Rollback -----------------------------------------------------------------

func TestRollback_Success(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.prev, prevContent)
	origPrev, err := os.Stat(h.prev)
	if err != nil {
		t.Fatal(err)
	}
	h.set(func() { h.prevOut = "krill v0.0.9\n" })

	if err := h.u.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	h.u.wg.Wait()

	if st := h.u.Status(); st.Phase != PhaseRestarting || st.Tag != "v0.0.9" || st.Err != nil {
		t.Errorf("Status() = %+v, want restarting v0.0.9", st)
	}
	assertSameFile(t, h.bin, origPrev)
	assertSameFile(t, h.prev, h.origBin)
	assertCmds(t, h.runner.cmds(), stopTimerCmd, restartCmd)
	assertMissing(t, h.pending)
	assertNoStaged(t, h.binDir)
	var m struct{ From, To string }
	readJSON(t, filepath.Join(h.runDir, rolledbackMarkerName), &m)
	if m.From != testCurrent || m.To != "v0.0.9" {
		t.Errorf("rolledback marker = %+v, want from %s to v0.0.9", m, testCurrent)
	}
}

func TestRollback_RestartFails(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.prev, prevContent)
	origPrev, err := os.Stat(h.prev)
	if err != nil {
		t.Fatal(err)
	}
	h.set(func() { h.prevOut = "krill v0.0.9\n" })
	h.runner.fail = func(cmd string, _ int) bool { return cmd == restartCmd }

	if err := h.u.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	h.u.wg.Wait()

	assertFailed(t, h.u.Status(), "restart")
	assertCmds(t, h.runner.cmds(), stopTimerCmd, restartCmd, stopTimerCmd)
	assertSameFile(t, h.bin, h.origBin)
	assertSameFile(t, h.prev, origPrev)
	assertMissing(t, filepath.Join(h.runDir, rolledbackMarkerName))
	assertNoStaged(t, h.binDir)
}

func TestRollback_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, h *harness)
		check func(t *testing.T, err error)
	}{
		{"no previous binary", func(*testing.T, *harness) {}, wantIs(ErrNoPrevious)},
		{"previous prints garbage", func(t *testing.T, h *harness) {
			writeFile(t, h.prev, prevContent)
			h.prevOut = "Segmentation fault\n"
		}, wantIs(ErrNoPrevious)},
		{"unsupported", func(t *testing.T, h *harness) {
			writeFile(t, h.prev, prevContent)
			h.prevOut = "krill v0.0.9\n"
			h.u.geteuid = func() int { return 1000 }
		}, func(t *testing.T, err error) {
			if !errors.Is(err, ErrUnsupported) || !strings.HasSuffix(err.Error(), ": root") {
				t.Errorf("err = %v, want ErrUnsupported with reason root", err)
			}
		}},
		{"pending marker", func(t *testing.T, h *harness) {
			writeFile(t, h.prev, prevContent)
			h.prevOut = "krill v0.0.9\n"
			writeFile(t, h.pending, "{}")
		}, wantIs(ErrPending)},
		{"busy", func(t *testing.T, h *harness) {
			writeFile(t, h.prev, prevContent)
			h.prevOut = "krill v0.0.9\n"
			h.u.busy = func(context.Context) string { return "db_migration" }
		}, func(t *testing.T, err error) {
			var be *BusyError
			if !errors.As(err, &be) || be.Reason != "db_migration" {
				t.Errorf("err = %v, want BusyError{db_migration}", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.set(func() { tc.setup(t, h) })
			tc.check(t, h.u.Rollback())
			h.u.wg.Wait()
			assertCmds(t, h.runner.cmds())
			assertSameFile(t, h.bin, h.origBin)
			if st := h.u.Status(); st.Phase != PhaseIdle {
				t.Errorf("Status() = %+v, want idle", st)
			}
		})
	}
}

func TestUndoSwap_HalfDone(t *testing.T) {
	dir := t.TempDir()
	bin, prev, tmp := filepath.Join(dir, "krill"), filepath.Join(dir, "krill.prev"), filepath.Join(dir, ".krill-rollback.tmp")
	writeFile(t, bin, oldContent)
	writeFile(t, prev, prevContent)
	origBin, _ := os.Stat(bin)
	origPrev, _ := os.Stat(prev)

	// Stage 1: prev was renamed onto bin, bin's old inode waits in tmp.
	if err := os.Rename(bin, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(prev, bin); err != nil {
		t.Fatal(err)
	}
	undoSwap(bin, prev, tmp, 1)

	assertSameFile(t, bin, origBin)
	assertSameFile(t, prev, origPrev)
	assertMissing(t, tmp)
}

func TestSwapWithPrev(t *testing.T) {
	dir := t.TempDir()
	bin, prev, tmp := filepath.Join(dir, "krill"), filepath.Join(dir, "krill.prev"), filepath.Join(dir, ".krill-rollback.tmp")
	writeFile(t, bin, oldContent)
	writeFile(t, prev, prevContent)
	writeFile(t, tmp, "stale")
	origBin, _ := os.Stat(bin)
	origPrev, _ := os.Stat(prev)

	stage, err := swapWithPrev(bin, prev, tmp)
	if err != nil || stage != 2 {
		t.Fatalf("swapWithPrev() = (%d, %v), want (2, nil)", stage, err)
	}
	assertSameFile(t, bin, origPrev)
	assertSameFile(t, prev, origBin)
	assertMissing(t, tmp)

	// Without a prev nothing moves.
	if err := os.Remove(prev); err != nil {
		t.Fatal(err)
	}
	stage, err = swapWithPrev(bin, prev, tmp)
	if err == nil || stage != 0 {
		t.Fatalf("swapWithPrev() without prev = (%d, %v), want stage 0 and an error", stage, err)
	}
	assertSameFile(t, bin, origPrev)
}

// copyFile is linkOrCopy's fallback where the filesystem refuses hard links.
func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "krill"), filepath.Join(dir, "krill.prev")
	if err := os.WriteFile(src, []byte(oldContent), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile() error = %v", err)
	}
	assertContent(t, dst, oldContent)
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("copy mode = %v, want 0755", fi.Mode().Perm())
	}
	srcInfo, _ := os.Stat(src)
	if os.SameFile(fi, srcInfo) {
		t.Error("copyFile() produced a link, want a separate file")
	}
	assertNoStaged(t, dir)
}

// A copy that fails partway must leave the destination as it was and no
// temporary file behind — never a truncated binary under the real name.
func TestCopyFile_FailureLeavesDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "krill.prev")
	writeFile(t, dst, prevContent)
	srcDir := filepath.Join(dir, "not-a-file")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(srcDir, dst); err == nil {
		t.Fatal("copyFile() from a directory succeeded, want an error")
	}
	assertContent(t, dst, prevContent)
	assertNoStaged(t, dir)
}

func TestLinkOrCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "krill")
	writeFile(t, src, oldContent)
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	orig := linkFile
	t.Cleanup(func() { linkFile = orig })

	t.Run("hard link", func(t *testing.T) {
		dst := filepath.Join(dir, "linked")
		if err := linkOrCopy(src, dst); err != nil {
			t.Fatalf("linkOrCopy() error = %v", err)
		}
		assertSameFile(t, dst, srcInfo)
	})
	for _, errno := range []syscall.Errno{syscall.EXDEV, syscall.EPERM} {
		t.Run("copy on "+errno.Error(), func(t *testing.T) {
			linkFile = func(o, n string) error { return &os.LinkError{Op: "link", Old: o, New: n, Err: errno} }
			defer func() { linkFile = orig }()
			dst := filepath.Join(dir, "copied-"+strconv.Itoa(int(errno)))
			if err := linkOrCopy(src, dst); err != nil {
				t.Fatalf("linkOrCopy() error = %v", err)
			}
			assertContent(t, dst, oldContent)
			if fi, err := os.Stat(dst); err != nil || os.SameFile(fi, srcInfo) {
				t.Errorf("dst = (%v, %v), want a separate copy", fi, err)
			}
			assertNoStaged(t, dir)
		})
	}
	t.Run("other errors are returned", func(t *testing.T) {
		linkFile = func(o, n string) error { return &os.LinkError{Op: "link", Old: o, New: n, Err: syscall.EACCES} }
		defer func() { linkFile = orig }()
		dst := filepath.Join(dir, "refused")
		if err := linkOrCopy(src, dst); !errors.Is(err, syscall.EACCES) {
			t.Errorf("linkOrCopy() = %v, want EACCES", err)
		}
		assertMissing(t, dst)
	})
}

// --- Previous -----------------------------------------------------------------

func TestPrevious(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if v, ok := h.u.Previous(ctx); ok || v != "" {
		t.Errorf("Previous() without a prev = (%q, %v), want (\"\", false)", v, ok)
	}

	writeFile(t, h.prev, prevContent)
	h.set(func() { h.prevOut = "krill v0.0.9\n" })
	for i := 0; i < 2; i++ {
		if v, ok := h.u.Previous(ctx); !ok || v != "v0.0.9" {
			t.Fatalf("Previous() = (%q, %v), want (v0.0.9, true)", v, ok)
		}
	}
	if n := h.execCount(); n != 1 {
		t.Errorf("exec calls for two Previous() = %d, want 1", n)
	}

	// Replace prev with another binary: the cache must not survive it.
	staged := filepath.Join(h.binDir, "replacement")
	writeFile(t, staged, prevContent+"-longer")
	if err := os.Rename(staged, h.prev); err != nil {
		t.Fatal(err)
	}
	h.set(func() { h.prevOut = "krill v0.0.8\n" })
	if v, ok := h.u.Previous(ctx); !ok || v != "v0.0.8" {
		t.Errorf("Previous() after replacing prev = (%q, %v), want (v0.0.8, true)", v, ok)
	}
	if n := h.execCount(); n != 2 {
		t.Errorf("exec calls = %d, want 2", n)
	}
}

func TestPrevious_Garbage(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.prev, prevContent)
	h.set(func() { h.prevOut = "krill dev+abc\n" })
	if v, ok := h.u.Previous(context.Background()); ok || v != "" {
		t.Errorf("Previous() = (%q, %v), want (\"\", false)", v, ok)
	}
}

// --- helpers --------------------------------------------------------------------

func TestStreamToFile(t *testing.T) {
	dir := t.TempDir()
	body := []byte("0123456789")

	t.Run("within the limit", func(t *testing.T) {
		path := filepath.Join(dir, "ok")
		sum, err := streamToFile(bytes.NewReader(body), path, int64(len(body)))
		if err != nil {
			t.Fatalf("streamToFile() error = %v", err)
		}
		if sum != sha256Hex(body) {
			t.Errorf("sum = %s, want %s", sum, sha256Hex(body))
		}
		assertContent(t, path, string(body))
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
		}
	})
	t.Run("over the limit", func(t *testing.T) {
		path := filepath.Join(dir, "big")
		if _, err := streamToFile(bytes.NewReader(body), path, int64(len(body)-1)); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Errorf("streamToFile() error = %v, want too large", err)
		}
	})
	t.Run("refuses an existing file", func(t *testing.T) {
		path := filepath.Join(dir, "exists")
		writeFile(t, path, "keep")
		if _, err := streamToFile(bytes.NewReader(body), path, 100); err == nil {
			t.Error("streamToFile() over an existing file succeeded, want O_EXCL to refuse")
		}
		assertContent(t, path, "keep")
	})
}

func TestRunVersion_EmptyEnvAndBoundedOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Setenv("KRILL_TEST_SECRET", "hunter2")
	script := filepath.Join(t.TempDir(), "fake-krill")
	body := "#!/bin/sh\n" +
		"echo \"krill v9.9.9 arg=$1 pwd=$(pwd) secret=${KRILL_TEST_SECRET:-unset}\"\n" +
		"i=0; while [ $i -lt 2000 ]; do echo 'filler filler filler' >&2; i=$((i+1)); done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runVersion(context.Background(), script)
	if err != nil {
		t.Fatalf("runVersion() error = %v", err)
	}
	if want := "krill v9.9.9 arg=--version pwd=/ secret=unset"; firstLine(out) != want {
		t.Errorf("first line = %q, want %q", firstLine(out), want)
	}
	if len(out) > 4<<10 {
		t.Errorf("output length = %d, want at most 4 KiB", len(out))
	}
}
