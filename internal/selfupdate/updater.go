package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"github.com/proshik/krill/internal/buildinfo"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/netguard"
)

// Phase is the state of the self-update job.
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhaseDownloading Phase = "downloading"
	PhaseVerifying   Phase = "verifying"
	PhaseInstalling  Phase = "installing"
	PhaseRestarting  Phase = "restarting"
	PhaseFailed      Phase = "failed"
)

// Active reports whether a job is in progress in this phase. Restarting
// counts: the job succeeded and the process is about to be replaced.
func (p Phase) Active() bool {
	switch p {
	case PhaseDownloading, PhaseVerifying, PhaseInstalling, PhaseRestarting:
		return true
	}
	return false
}

// Status is a snapshot of the current (or last) self-update job.
type Status struct {
	Phase     Phase
	Tag       string // the version being installed (or rolled back to)
	Err       error  // set when Phase is PhaseFailed
	StartedAt time.Time
}

// Result is the outcome of the previous process's update, as found on disk
// by ConfirmStartup.
type Result struct {
	From, To string
	OK       bool
	Reason   string // "" (updated), "reverted", "interrupted", "rollback"
}

var (
	ErrUnsupported = errors.New("selfupdate: self-update is not supported on this host")
	ErrJobRunning  = errors.New("selfupdate: an update is already in progress")
	ErrInvalidTag  = errors.New("selfupdate: invalid release tag")
	ErrNotLatest   = errors.New("selfupdate: the latest release changed, check again")
	ErrNotNewer    = errors.New("selfupdate: the release is not newer than the running version")
	ErrPending     = errors.New("selfupdate: a previous update is still waiting for confirmation")
	ErrNoPrevious  = errors.New("selfupdate: no previous version to roll back to")
)

// BusyError refuses an update or rollback while other work that a restart
// would interrupt is in flight. Reason is a short machine code supplied by
// the busy callback (e.g. "deploy").
type BusyError struct{ Reason string }

func (e *BusyError) Error() string {
	return "selfupdate: busy: " + e.Reason
}

// Updater installs a newer Krill release over the running binary, guarded by
// a dead-man timer that puts the previous binary back unless the new process
// confirms its own startup (ConfirmStartup).
type Updater struct {
	runner  firewall.Runner
	checker *Checker
	repo    string
	busy    func(context.Context) string

	// Everything below is overridden directly by tests in this package;
	// there are deliberately no exported setters.
	baseURL     string
	client      *http.Client
	current     string
	binPath     string
	runDir      string
	unitDir     string
	unit        string
	revertUnit  string
	restartUnit string
	revertDelay time.Duration
	jobTimeout  time.Duration
	stopBackoff time.Duration // ConfirmStartup sleeps stopBackoff*attempt between stop-timer retries
	confirmWait time.Duration // how long ConfirmStartup waits for the job slot before going ahead without it
	confirmPoll time.Duration // how often it retries the slot meanwhile
	execVersion func(ctx context.Context, path string) (string, error)
	freeBytes   func(dir string) (uint64, error)
	goos        string
	goarch      string
	getenv      func(string) string
	getppid     func() int
	geteuid     func() int
	now         func() time.Time

	mu        sync.Mutex
	status    Status
	claimed   bool // a Start/Rollback passed the job-active check and owns the job slot
	last      *Result
	prevCache prevVersionCache
	wg        sync.WaitGroup
}

// NewUpdater creates an Updater. runner executes host shell commands (the
// same firewall.LocalRunner the control-plane firewall uses); checker supplies
// the cached latest release Start insists on; repo is "owner/repo";
// allowPrivate is forwarded to the netguard egress guard; busy, when non-nil,
// returns a non-empty reason while a restart must not happen.
func NewUpdater(runner firewall.Runner, checker *Checker, repo string, allowPrivate bool,
	busy func(context.Context) string) *Updater {
	bin := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			bin = resolved
		}
	}
	unit := ""
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		unit = unitFromCgroup(string(data))
	}
	if unit == "" {
		unit = "krill.service"
	}
	return &Updater{
		runner:  runner,
		checker: checker,
		repo:    repo,
		busy:    busy,
		baseURL: "https://github.com",
		client: &http.Client{
			// Release downloads redirect to GitHub's object storage; the
			// redirect target is dialed through the same egress guard.
			Transport: &http.Transport{
				DialContext:           netguard.DialContext(allowPrivate),
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		current:     buildinfo.String(),
		binPath:     bin,
		runDir:      "/run",
		unitDir:     "/etc/systemd/system",
		unit:        unit,
		revertUnit:  "krill-update-revert",
		restartUnit: "krill-update-restart",
		revertDelay: 10 * time.Minute,
		jobTimeout:  15 * time.Minute,
		stopBackoff: time.Second,
		confirmWait: time.Minute,
		confirmPoll: 50 * time.Millisecond,
		execVersion: runVersion,
		freeBytes:   diskFree,
		goos:        runtime.GOOS,
		goarch:      runtime.GOARCH,
		getenv:      os.Getenv,
		getppid:     os.Getppid,
		geteuid:     os.Geteuid,
		now:         time.Now,
		status:      Status{Phase: PhaseIdle},
	}
}

// Status returns a snapshot of the current or last job.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.status
	if st.Phase == "" {
		st.Phase = PhaseIdle
	}
	return st
}

// LastResult returns what ConfirmStartup found about the previous process's
// update, and false when there was nothing to report.
func (u *Updater) LastResult() (Result, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil {
		return Result{}, false
	}
	return *u.last, true
}

// claim takes the single job slot. It fails while another Start/Rollback is
// validating or running, and after a job reached Restarting (the process is
// about to be replaced). Holding the slot through the remaining refusal
// checks — instead of flipping the phase early — keeps Status from showing a
// job that is then refused.
func (u *Updater) claim() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.claimed || u.status.Phase.Active() {
		return false
	}
	u.claimed = true
	return true
}

func (u *Updater) release() {
	u.mu.Lock()
	u.claimed = false
	u.mu.Unlock()
}

func (u *Updater) setPhase(p Phase) {
	u.mu.Lock()
	u.status.Phase = p
	u.mu.Unlock()
}

// Busy returns the reason an update or rollback would be refused right now
// because of other work in flight ("deploy", "backup", ...), or "" when
// nothing is running or no busy callback is wired. The UI uses it to disable
// its buttons and say why; ctx bounds the check.
func (u *Updater) Busy(ctx context.Context) string {
	if u.busy == nil {
		return ""
	}
	return u.busy(ctx)
}

// busyReason asks the busy callback, bounded so a stuck database cannot hang
// a Start.
func (u *Updater) busyReason() string {
	if u.busy == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return u.busy(ctx)
}

// Start validates an update to tag and, when every check passes, runs the
// install job in the background (watch it through Status). Refusals, in
// order: ErrUnsupported, ErrJobRunning, ErrInvalidTag, ErrNotLatest (tag is
// not the checker's cached latest release), ErrNotNewer, ErrPending, and a
// *BusyError. The job asks the busy callback once more after the download,
// before it changes anything on the host, and fails with a *BusyError
// (reported through Status) if work has started in the meantime.
func (u *Updater) Start(tag string) error {
	if ok, reason := u.Supported(); !ok {
		return fmt.Errorf("%w: %s", ErrUnsupported, reason)
	}
	if !u.claim() {
		return ErrJobRunning
	}
	if err := u.checkStart(tag); err != nil {
		u.release()
		return err
	}
	slog.Info("self-update started", "from", u.current, "to", tag)
	u.launch(PhaseDownloading, tag, "self-update failed", func(ctx context.Context) error {
		return u.install(ctx, tag)
	})
	return nil
}

func (u *Updater) checkStart(tag string) error {
	if !ValidTag(tag) {
		return ErrInvalidTag
	}
	if u.checker == nil || u.checker.State().Latest.Tag != tag {
		return ErrNotLatest
	}
	if !Newer(u.current, tag) {
		return ErrNotNewer
	}
	if markerExists(u.markerPath(pendingMarkerName)) {
		return ErrPending
	}
	if reason := u.busyReason(); reason != "" {
		return &BusyError{Reason: reason}
	}
	return nil
}

// Rollback swaps the running binary with <bin>.prev and restarts — for a new
// release that serves the UI but is broken. No revert timer is armed: the
// target already ran here, and a timer would bring the broken one back.
// Refusals, in order: ErrUnsupported, ErrJobRunning, ErrPending,
// ErrNoPrevious (no .prev, or it does not report a release older than the
// running version), and a *BusyError. Like Start's job, the rollback job
// asks the busy callback again right before the swap.
func (u *Updater) Rollback() error {
	if ok, reason := u.Supported(); !ok {
		return fmt.Errorf("%w: %s", ErrUnsupported, reason)
	}
	if !u.claim() {
		return ErrJobRunning
	}
	version, err := u.checkRollback()
	if err != nil {
		u.release()
		return err
	}
	slog.Info("self-update rollback started", "from", u.current, "to", version)
	u.launch(PhaseInstalling, version, "self-update rollback failed", func(ctx context.Context) error {
		return u.rollback(ctx, version)
	})
	return nil
}

func (u *Updater) checkRollback() (string, error) {
	if markerExists(u.markerPath(pendingMarkerName)) {
		return "", ErrPending
	}
	prev := u.binPath + prevSuffix
	if _, err := os.Stat(prev); err != nil {
		return "", ErrNoPrevious
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := u.execVersion(ctx, prev)
	version, ok := parseVersion(out)
	if err != nil || !ok {
		slog.Warn("self-update: the previous binary failed its smoke test", "path", prev, "output", truncate(out, 200), "err", err)
		return "", ErrNoPrevious
	}
	if !u.olderThanCurrent(version) {
		slog.Warn("self-update: the previous binary is not older than the running one", "path", prev, "previous", version, "running", u.current)
		return "", ErrNoPrevious
	}
	if reason := u.busyReason(); reason != "" {
		return "", &BusyError{Reason: reason}
	}
	return version, nil
}

// launch records the job's start and runs job in a goroutine with the job
// timeout. The caller holds the job slot; launch hands it to the goroutine,
// which gives it back when the job ends.
func (u *Updater) launch(phase Phase, tag, failMsg string, job func(context.Context) error) {
	u.mu.Lock()
	u.status = Status{Phase: phase, Tag: tag, StartedAt: u.now()}
	u.mu.Unlock()

	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer u.release()
		ctx, cancel := context.WithTimeout(context.Background(), u.jobTimeout)
		defer cancel()
		if err := job(ctx); err != nil {
			u.mu.Lock()
			phase := u.status.Phase
			u.status.Phase = PhaseFailed
			u.status.Err = err
			u.mu.Unlock()
			slog.Error(failMsg, "to", tag, "phase", phase, "err", err)
		}
	}()
}

// Previous returns the version <bin>.prev reports ("v0.1.3"), or false when
// there is no previous binary, it does not report a release version, or that
// version is not older than the running one. The parsed version is cached per
// file so page renders do not exec it every time.
func (u *Updater) Previous(ctx context.Context) (string, bool) {
	if u.binPath == "" {
		return "", false
	}
	prev := u.binPath + prevSuffix
	fi, err := os.Stat(prev)
	if err != nil {
		return "", false
	}
	u.mu.Lock()
	cache := u.prevCache
	u.mu.Unlock()
	version, ok := cache.version, cache.ok
	if !cache.matches(fi) {
		out, err := u.execVersion(ctx, prev)
		version, ok = parseVersion(out)
		if err != nil {
			version, ok = "", false
		}
		// A run cut short by the caller says nothing about the file.
		if ctx.Err() == nil {
			u.mu.Lock()
			u.prevCache = prevVersionCache{info: fi, version: version, ok: ok}
			u.mu.Unlock()
		}
	}
	if !ok || !u.olderThanCurrent(version) {
		return "", false
	}
	return version, true
}

// olderThanCurrent reports whether version, a release tag, is strictly older
// than the running version. The rollback guarantee covers only going back to
// an earlier release: a <bin>.prev left from an update several releases ago,
// with the binary since replaced some other way, may be the running version
// or newer. A running version that is not valid semver (a "dev" build) ranks
// below every release, so nothing counts as older than it.
func (u *Updater) olderThanCurrent(version string) bool {
	return semver.Compare(version, u.current) < 0
}

// parseVersion extracts the version from `krill --version` output: the first
// line must be exactly "krill vX.Y.Z".
func parseVersion(out string) (string, bool) {
	v, found := strings.CutPrefix(firstLine(out), "krill ")
	if !found || !ValidTag(v) {
		return "", false
	}
	return v, true
}

// firstLine returns the first line of s with surrounding space trimmed.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
