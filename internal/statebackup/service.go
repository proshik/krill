package statebackup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
)

const (
	// RunTimeout bounds a scheduled or manual run.
	RunTimeout = 30 * time.Minute
	// SnapshotTimeout bounds the snapshot taken before a self-update.
	SnapshotTimeout = 10 * time.Minute
	// StateDumpLabelValue is the docker.OneOffRoleLabel value of the dump
	// container, so a leftover one can be removed at the next start.
	StateDumpLabelValue = "state-dump"
)

// minServerMajor is the oldest Postgres major the dump supports.
const minServerMajor = 13

// cleanupTimeout bounds the deletion of an unverified object, which runs on a
// fresh context: the run's own may be what expired.
const cleanupTimeout = 30 * time.Second

// errTextCap bounds the external text (runner stderr, an S3 error) carried in
// a run's error.
const errTextCap = 1024

var (
	ErrStateBackupRunning = errors.New("statebackup: a backup of Krill's state is already running")
	ErrNotConfigured      = errors.New("statebackup: Krill backups are not set up or are paused")
	ErrServerTooOld       = errors.New("statebackup: the database server is older than PostgreSQL 13")
	ErrNoRunner           = errors.New("statebackup: this Docker engine cannot run the dump container")
	ErrUpdateInProgress   = errors.New("statebackup: a Krill update is in progress")
	ErrClientTooOld       = errors.New("statebackup: the database URL needs a newer pg_dump than the one matching the database server (sslnegotiation=direct needs PostgreSQL 17)")

	errSettings = errors.New("statebackup: the settings could not be read")
)

// Querier reads the database server's version (*pgxpool.Pool satisfies it).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Notifier receives failed runs (implemented by *notify.Service). The reason
// is the run's error text, already free of secrets.
type Notifier interface {
	StateBackupFailed(ctx context.Context, trigger, from, to, reason string)
}

// Service dumps Krill's control-plane database to S3.
type Service struct {
	q            Querier
	runner       docker.OneOffRunner
	store        Store
	dsn          string // KRILL_DATABASE_URL; never logged or shown
	allowPrivate bool   // KRILL_ALLOW_PRIVATE_EGRESS
	caBundles    []string
	notifier     Notifier
	onChange     func()
	updateActive func() bool
	// head reads a stored object's size (backup.Head; a test hook).
	head func(ctx context.Context, d backup.Destination, key string, allowPrivate bool) (int64, error)

	mu       sync.Mutex
	inFlight bool
}

// New builds the service. A nil runner is allowed; every run then fails with
// ErrNoRunner.
func New(q Querier, runner docker.OneOffRunner, store Store, dsn string, allowPrivate bool) *Service {
	return &Service{q: q, runner: runner, store: store, dsn: dsn, allowPrivate: allowPrivate, caBundles: DefaultCABundles, head: backup.Head}
}

// SetNotifier wires failure alerts (no-op if never set).
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// SetUpdateActive registers f, which reports whether a self-update job is
// active. A manual or scheduled run is refused meanwhile: the updater's last
// busy check may already be behind it, and its restart would cut the run
// short. The update's own Snapshot is not affected.
func (s *Service) SetUpdateActive(f func() bool) { s.updateActive = f }

// SetOnChange registers f, called after the settings were saved or the
// feature was turned on or off (the scheduler's Reload).
func (s *Service) SetOnChange(f func()) { s.onChange = f }

func (s *Service) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

// Save validates in, checks that the storage is reachable with it, and only
// then stores it.
func (s *Service) Save(ctx context.Context, in Input) error {
	stored, err := s.store.Load(ctx)
	if errors.Is(err, secret.ErrUndecryptable) {
		// The kept key cannot be used: the form must supply it again.
		stored, err = s.store.LoadView(ctx)
		stored.HasSecretKey = false
	}
	if err != nil {
		slog.Error("state backup: reading the settings failed", "err", err)
		return errSettings
	}
	out, err := Prepare(stored, in)
	if err != nil {
		return err
	}
	if err := backup.CheckAccess(ctx, out.Destination(), s.allowPrivate); err != nil {
		// The text is shown on the page: no secret, capped like run errors.
		// The cause stays reachable for errors.Is (e.g. netguard.ErrBlocked).
		msg := capText(redactAll(err.Error(), out.SecretKey))
		return &StorageError{Msg: msg, cause: err}
	}
	if err := s.store.Save(ctx, out); err != nil {
		return err
	}
	s.changed()
	return nil
}

// SetEnabled pauses or resumes the state backup.
func (s *Service) SetEnabled(ctx context.Context, on bool) error {
	if err := s.store.SetEnabled(ctx, on); err != nil {
		return err
	}
	s.changed()
	return nil
}

// View returns the settings without decrypting the secret key.
func (s *Service) View(ctx context.Context) (Settings, error) { return s.store.LoadView(ctx) }

// Runs returns the newest n runs.
func (s *Service) Runs(ctx context.Context, n int) ([]Run, error) { return s.store.ListRuns(ctx, n) }

// LastSuccess returns the newest successful run.
func (s *Service) LastSuccess(ctx context.Context) (Run, bool, error) {
	return s.store.LastSuccess(ctx)
}

// InFlight reports whether a run holds the slot (0 or 1). The self-updater
// refuses to restart Krill while it does.
func (s *Service) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return 1
	}
	return 0
}

// claimRun reserves the slot for a manual or scheduled run, refusing while a
// self-update is active. The update is asked with the slot already held, so
// an update starting after the check still finds the slot taken (busy).
func (s *Service) claimRun() (release func(), err error) {
	release, ok := s.claim()
	if !ok {
		return nil, ErrStateBackupRunning
	}
	if s.updateActive != nil && s.updateActive() {
		release()
		return nil, ErrUpdateInProgress
	}
	return release, nil
}

// claim reserves the service's single slot; release must be called when the
// run ends.
func (s *Service) claim() (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return nil, false
	}
	s.inFlight = true
	return func() {
		s.mu.Lock()
		s.inFlight = false
		s.mu.Unlock()
	}, true
}

// RemoveLeftovers removes the dump containers a crashed run left behind (a
// running one's pg_dump still holds the database password in its process
// environment) and returns how many
// went. It holds the run slot while it sweeps, so it never removes the
// container of a run in flight, and a run cannot start meanwhile; with the
// slot taken it removes nothing and returns (0, nil).
func (s *Service) RemoveLeftovers(ctx context.Context) (int, error) {
	if s.runner == nil {
		return 0, nil
	}
	release, ok := s.claim()
	if !ok {
		slog.Info("state backup: leftover container sweep skipped: a run is in flight")
		return 0, nil
	}
	defer release()
	return s.runner.RemoveContainersByLabel(ctx, docker.OneOffRoleLabel, StateDumpLabelValue)
}

func checkTrigger(t Trigger) error {
	if t != TriggerManual && t != TriggerSchedule {
		return fmt.Errorf("statebackup: a run cannot be started with trigger %q", t)
	}
	return nil
}

// allows reports whether v permits a run started by t: a manual run needs
// only storage and a recipient (it works while paused); an automatic one also
// needs the state backup to be enabled.
func allows(v Settings, t Trigger) bool {
	if t == TriggerManual {
		return v.Ready()
	}
	return v.Configured()
}

// configured reports whether the stored settings allow a run started by t,
// without decrypting anything.
func (s *Service) configured(ctx context.Context, t Trigger) (bool, error) {
	v, err := s.store.LoadView(ctx)
	if err != nil {
		slog.Error("state backup: reading the settings failed", "err", err)
		return false, errSettings
	}
	return allows(v, t), nil
}

// Start runs a backup in the background, bounded by RunTimeout. It returns
// ErrStateBackupRunning when the slot is taken, ErrUpdateInProgress while a
// self-update is active and ErrNotConfigured when there is nothing to run,
// and nil once the run has started.
func (s *Service) Start(trigger Trigger) error {
	if err := checkTrigger(trigger); err != nil {
		return err
	}
	release, err := s.claimRun()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), RunTimeout)
	ok, err := s.configured(ctx, trigger)
	if err == nil && !ok {
		err = ErrNotConfigured
	}
	if err != nil {
		cancel()
		release()
		return err
	}
	go func() {
		defer release()
		defer cancel()
		_ = s.run(ctx, trigger, "", "") // a failure is journalled, alerted and logged by run
	}()
	return nil
}

// Run runs a backup synchronously, bounded by RunTimeout.
func (s *Service) Run(ctx context.Context, trigger Trigger) error {
	if err := checkTrigger(trigger); err != nil {
		return err
	}
	release, err := s.claimRun()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	return s.run(ctx, trigger, "", "")
}

// Snapshot takes the backup that precedes a self-update from one release to
// another, bounded by SnapshotTimeout. It reports false with no error when the
// state backup is not set up or is paused; any failure, an undecryptable
// secret key included, is an error that must block the update.
func (s *Service) Snapshot(ctx context.Context, from, to string) (bool, error) {
	ok, err := s.configured(ctx, TriggerPreUpdate)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	release, ok := s.claim()
	if !ok {
		return false, ErrStateBackupRunning
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, SnapshotTimeout)
	defer cancel()
	if err := s.run(ctx, TriggerPreUpdate, from, to); err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// runError is a failed run's error. Its text is built from fixed messages and
// capped external text with every known secret removed; it unwraps only to a
// sentinel, never to a raw cause that could echo a DSN or a credential.
type runError struct {
	msg      string
	sentinel error
}

func (e *runError) Error() string { return e.msg }
func (e *runError) Unwrap() error { return e.sentinel }

// StorageError is a Save refusal because the storage could not be reached with
// the entered settings. Msg is shown on the page: it carries no secret and is
// capped. The cause stays reachable for errors.Is (e.g. netguard.ErrBlocked).
type StorageError struct {
	Msg   string
	cause error
}

func (e *StorageError) Error() string {
	return "statebackup: the storage could not be reached: " + e.Msg
}
func (e *StorageError) Unwrap() error { return e.cause }

// redactAll replaces every non-empty secret in t.
func redactAll(t string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			t = strings.ReplaceAll(t, sec, "[redacted]")
		}
	}
	return t
}

// capText shortens external text to errTextCap bytes on a rune boundary.
func capText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= errTextCap {
		return s
	}
	cut := errTextCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ImageFor is the image whose pg_dump matches a server_version_num.
func ImageFor(versionNum int) (string, error) {
	major := versionNum / 10000
	if major < minServerMajor {
		return "", fmt.Errorf("%w (it runs version %d)", ErrServerTooOld, major)
	}
	return fmt.Sprintf("postgres:%d-alpine", major), nil
}

// DumpArgs is the dump command. There is no --clean: a restore goes into an
// empty database. The excluded table data is never worth restoring, and
// sessions must never leave the server at all: its primary key is a live
// session token. Any future table that stores a live bearer credential in
// plain form belongs on this list too.
func DumpArgs() []string {
	return []string{
		"pg_dump", "--format=plain", "--no-owner", "--no-privileges", "--lock-wait-timeout=60s",
		"--exclude-table-data=*.sessions", "--exclude-table-data=*.metric_samples",
	}
}

// NextRun is the next time schedule fires after now, in now's location; false
// for an empty or invalid schedule.
func NextRun(schedule string, now time.Time) (time.Time, bool) {
	if schedule == "" {
		return time.Time{}, false
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return time.Time{}, false
	}
	return sched.Next(now), true
}

// SchedStore adapts the settings to backup.Scheduler: one entry (ID 1) while
// the state backup is set up, enabled and has a schedule.
func (s *Service) SchedStore() backup.SchedStore { return schedStore{s} }

type schedStore struct{ s *Service }

func (a schedStore) ListEnabledBackups(ctx context.Context) ([]backup.SchedBackup, error) {
	v, err := a.s.store.LoadView(ctx)
	if err != nil {
		return nil, err
	}
	if !v.Configured() || v.Schedule == "" {
		return nil, nil
	}
	return []backup.SchedBackup{{ID: 1, Schedule: v.Schedule}}, nil
}

// run takes one backup. The caller holds the slot. ErrNotConfigured is
// returned without a journal row; every other outcome is journalled, and a
// failure is alerted.
func (s *Service) run(ctx context.Context, trigger Trigger, from, to string) error {
	rec := Run{Trigger: trigger, FromVersion: from, ToVersion: to, StartedAt: time.Now()}
	secrets := []string{s.dsn}
	redact := func(t string) string { return redactAll(t, secrets...) }
	// ext cleans external text (runner stderr, S3 errors): secrets go first,
	// so a cap can never leave part of one behind.
	ext := func(t string) string { return capText(redact(t)) }
	fail := func(sentinel error, msg string) error {
		msg = redact(msg)
		e := &runError{msg: msg, sentinel: sentinel}
		rec.Status, rec.Error, rec.FinishedAt = "error", msg, time.Now()
		s.finish(ctx, rec)
		slog.Error("state backup failed", "trigger", string(trigger), "err", msg)
		if s.notifier != nil {
			nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			s.notifier.StateBackupFailed(nctx, string(trigger), from, to, msg)
			cancel()
		}
		return e
	}

	// The caller already admitted the run, so a settings read that fails
	// here is a failed run (journalled and alerted); finding the backup
	// paused or not set up is still a refusal.
	ok, err := s.configured(ctx, trigger)
	if err != nil {
		return fail(errSettings, errSettings.Error())
	}
	if !ok {
		return ErrNotConfigured
	}

	st, err := s.store.Load(ctx)
	if errors.Is(err, secret.ErrUndecryptable) {
		return fail(secret.ErrUndecryptable, "the stored S3 secret key cannot be decrypted (KRILL_SECRET_KEY changed?): enter it again in the settings")
	}
	if err != nil {
		slog.Error("state backup: reading the settings failed", "err", err)
		return fail(errSettings, errSettings.Error())
	}
	if !allows(st, trigger) {
		return ErrNotConfigured // changed since the check above
	}
	secrets = append(secrets, st.SecretKey)
	if s.runner == nil {
		return fail(ErrNoRunner, ErrNoRunner.Error())
	}
	recipient, err := age.ParseX25519Recipient(st.Recipient)
	if err != nil {
		return fail(ErrInvalidRecipient, ErrInvalidRecipient.Error())
	}

	var version string
	if err := s.q.QueryRow(ctx, "SHOW server_version_num").Scan(&version); err != nil {
		return fail(nil, "the database server version could not be read")
	}
	num, err := strconv.Atoi(strings.TrimSpace(version))
	if err != nil {
		return fail(nil, "the database server reported an unreadable version")
	}
	image, err := ImageFor(num)
	if err != nil {
		return fail(ErrServerTooOld, err.Error())
	}

	conn, err := ConnFromDSN(s.dsn, s.caBundles)
	if errors.Is(err, ErrUnixSocket) {
		return fail(ErrUnixSocket, "the dump needs a TCP connection to the database: "+err.Error())
	}
	if err != nil {
		// Every ConnFromDSN error is a sentinel with a fixed message.
		return fail(err, err.Error())
	}
	if conn.MinClientMajor > num/10000 {
		return fail(ErrClientTooOld, ErrClientTooOld.Error())
	}
	secrets = append(secrets, conn.Password)
	nonce, err := newNonce()
	if err != nil {
		return fail(nil, "the dump's completion marker could not be generated")
	}

	key := ScheduledKey(st.Prefix, rec.StartedAt)
	if trigger == TriggerPreUpdate {
		key = PreUpdateKey(st.Prefix, rec.StartedAt, from, to)
	}
	dst := st.Destination()
	spec := docker.OneOffSpec{
		Image: image, Cmd: dumpCmd(nonce), Env: conn.Env, HostNetwork: true,
		// The password travels on stdin only: Env is in the create request
		// and the container's config (docker inspect, a debug daemon log).
		Stdin:         strings.NewReader(conn.Password + "\n"),
		ReadOnlyFiles: conn.Files,
		Labels:        map[string]string{docker.OneOffRoleLabel: StateDumpLabelValue},
	}

	n, msg := s.stream(ctx, spec, recipient, dst, key, completionMarker(nonce), ext)
	if msg != "" {
		return fail(nil, msg)
	}

	size, err := s.head(ctx, dst, key, s.allowPrivate)
	if err != nil || size != n {
		cause := ""
		if err != nil {
			cause = "the stored backup could not be checked: " + ext(err.Error())
		} else {
			cause = fmt.Sprintf("the storage holds %d bytes of the backup, but %d were sent", size, n)
		}
		// An unverified object must not count as one of the kept backups.
		// The check may have used up the run's deadline, so the delete gets
		// its own.
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		derr := backup.Delete(dctx, dst, key, s.allowPrivate)
		dcancel()
		if derr != nil {
			slog.Error("state backup: deleting an unverified object failed", "key", key, "err", ext(derr.Error()))
			return fail(nil, cause+"; deleting it failed too")
		}
		return fail(nil, cause+"; it was deleted")
	}

	// Retention runs after a successful upload: a failure leaves the run ok,
	// with a note, since the dump itself is stored.
	dir, preUpdate, keep := ScheduledDir(st.Prefix), false, st.Retention
	if trigger == TriggerPreUpdate {
		dir, preUpdate, keep = PreUpdateDir(st.Prefix), true, PreUpdateKeep
	}
	objs, lerr := backup.List(ctx, dst, dir, s.allowPrivate)
	deleteFailures := 0
	if lerr == nil {
		for _, o := range Prunable(objs, dir, preUpdate, keep, key) {
			if derr := backup.Delete(ctx, dst, o.Key, s.allowPrivate); derr != nil {
				deleteFailures++
				slog.Error("state backup retention delete failed", "key", o.Key, "err", ext(derr.Error()))
			}
		}
	}
	note := retentionNote(lerr, deleteFailures, ext)
	if note != "" {
		slog.Error("state backup retention failed", "err", note)
	}

	rec.Status, rec.ObjectKey, rec.SizeBytes, rec.Error, rec.FinishedAt = "ok", key, n, note, time.Now()
	s.finish(ctx, rec)
	slog.Info("state backup stored", "trigger", string(trigger), "key", key, "bytes", n)
	return nil
}

// stream runs the dump container and uploads its output, gzipped and
// age-encrypted, to key. It returns the bytes uploaded, or the text of the
// failure. Nothing unencrypted ever leaves the process.
func (s *Service) stream(ctx context.Context, spec docker.OneOffSpec, recipient age.Recipient, dst backup.Destination, key, marker string, ext func(string) string) (int64, string) {
	dumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	// The pipe does not observe ctx: once the run's context ends, fail both
	// ends so a read or write blocked on the other side returns on its own.
	// After a normal Close this changes nothing.
	stopPipeClose := context.AfterFunc(ctx, func() {
		pw.CloseWithError(ctx.Err())
		pr.CloseWithError(ctx.Err())
	})
	defer stopPipeClose()
	var (
		uploadFailed atomic.Bool
		dumpErr      string // written by the goroutine, read after done
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Every failure closes the pipe with an error BEFORE anything is
		// closed, so the uploader sees an error, never an EOF that would
		// complete a truncated object.
		failDump := func(msg string, err error) {
			if !uploadFailed.Load() {
				dumpErr = msg
			}
			pw.CloseWithError(err)
		}
		aw, err := age.Encrypt(pw, recipient)
		if err != nil {
			failDump("the encryption could not be started", err)
			return
		}
		gz := gzip.NewWriter(aw)
		tail := newMarkerTail(gz, marker)
		if err := s.runner.RunOneOff(dumpCtx, spec, tail); err != nil {
			failDump("the dump failed: "+ext(err.Error()), err)
			return
		}
		// A clean exit does not prove the whole output arrived: an attach
		// stream can end early. The wrapper prints this run's marker only
		// after pg_dump exited 0, and it must be the very last bytes.
		if !tail.complete() {
			failDump(errIncompleteDump.Error(), errIncompleteDump)
			return
		}
		if err := gz.Close(); err != nil {
			failDump("the compressed stream could not be finished", err)
			return
		}
		if err := aw.Close(); err != nil { // writes the final authenticated chunk
			failDump("the encrypted stream could not be finished", err)
			return
		}
		pw.Close()
	}()

	counted := &backup.CountingReader{R: pr}
	uerr := backup.Upload(ctx, dst, key, counted, s.allowPrivate)
	if uerr != nil {
		uploadFailed.Store(true)
		// Fail the goroutine's blocked write so the dump container stops.
		pr.CloseWithError(uerr)
		cancel()
	}
	<-done
	switch {
	case dumpErr != "":
		return 0, dumpErr
	case uerr != nil:
		return 0, "the upload failed: " + ext(uerr.Error())
	}
	return counted.N, ""
}

// dumpScript wraps pg_dump in the dump container (POSIX sh: busybox in the
// alpine images). It reads the password as the first line of stdin and
// exports it only when it is not empty, so it never appears in argv or the
// container's config, and prints the completion marker with the run's nonce
// ($1) only after pg_dump ("$@" after the shift) exited 0; a failure keeps
// pg_dump's exit status.
const dumpScript = `IFS= read -r p || { echo 'krill: the database password did not arrive on stdin' >&2; exit 97; }
if [ -n "$p" ]; then PGPASSWORD=$p; export PGPASSWORD; fi
unset p
n=$1
shift
"$@" || exit
printf '\n-- krill-state-backup-complete %s\n' "$n"`

// dumpCmd is the dump container's command for a run with nonce.
func dumpCmd(nonce string) []string {
	return append([]string{"sh", "-c", dumpScript, "krill-state-dump", nonce}, DumpArgs()...)
}

// completionMarker is what dumpScript prints after a successful pg_dump.
func completionMarker(nonce string) string {
	return "\n-- krill-state-backup-complete " + nonce + "\n"
}

// newNonce is a run's random marker nonce. It is not secret: it only makes
// the marker impossible to predict from the dumped data.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var errIncompleteDump = errors.New("the dump ended without this run's completion marker")

// markerTail passes the dump through to w, holding back its last len(marker)
// bytes: the marker never reaches the archive, and complete checks that the
// stream ended with exactly it.
type markerTail struct {
	w      io.Writer
	marker []byte
	hold   []byte
}

func newMarkerTail(w io.Writer, marker string) *markerTail {
	return &markerTail{w: w, marker: []byte(marker), hold: make([]byte, 0, 2*len(marker))}
}

func (t *markerTail) Write(p []byte) (int, error) {
	m := len(t.marker)
	if len(p) >= m {
		// Everything held plus all but the last m bytes of p is dump data.
		if len(t.hold) > 0 {
			if _, err := t.w.Write(t.hold); err != nil {
				return 0, err
			}
		}
		if _, err := t.w.Write(p[:len(p)-m]); err != nil {
			return 0, err
		}
		t.hold = append(t.hold[:0], p[len(p)-m:]...)
		return len(p), nil
	}
	t.hold = append(t.hold, p...)
	if over := len(t.hold) - m; over > 0 {
		if _, err := t.w.Write(t.hold[:over]); err != nil {
			return 0, err
		}
		t.hold = append(t.hold[:0], t.hold[over:]...)
	}
	return len(p), nil
}

// complete reports whether the stream ended with exactly the marker.
func (t *markerTail) complete() bool { return bytes.Equal(t.hold, t.marker) }

// finish journals rec on a context that survives the run's own deadline.
func (s *Service) finish(ctx context.Context, rec Run) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.store.RecordRun(rctx, rec); err != nil {
		slog.Error("state backup: journalling the run failed", "trigger", string(rec.Trigger), "status", rec.Status, "err", err)
	}
}

// retentionNote has backup's retention wording (unexported there); ext
// cleans the listing error's text.
func retentionNote(listErr error, deleteFailures int, ext func(string) string) string {
	if listErr != nil {
		return fmt.Sprintf("backup stored, but old backups could not be pruned (listing failed: %s) — the bucket will keep growing", ext(listErr.Error()))
	}
	if deleteFailures > 0 {
		return fmt.Sprintf("backup stored, but %d old backup(s) could not be deleted — the bucket will keep growing", deleteFailures)
	}
	return ""
}
