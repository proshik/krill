package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/statebackup"
	"github.com/proshik/krill/internal/testutil"
)

// gatedRunner holds every dump until release is called, so a run keeps the
// service's slot for as long as a test needs it.
type gatedRunner struct {
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once

	mu      sync.Mutex
	calls   int
	removed []string // key=value of each RemoveContainersByLabel call
	n       int
	rmErr   error
}

func newGatedRunner() *gatedRunner {
	return &gatedRunner{entered: make(chan struct{}, 8), gate: make(chan struct{})}
}

func (g *gatedRunner) RunOneOff(_ context.Context, _ docker.OneOffSpec, out io.Writer) error {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.entered <- struct{}{}
	<-g.gate
	_, err := out.Write([]byte("-- PostgreSQL database dump complete\n"))
	return err
}

func (g *gatedRunner) RemoveContainersByLabel(_ context.Context, key, value string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removed = append(g.removed, key+"="+value)
	return g.n, g.rmErr
}

func (g *gatedRunner) release() { g.once.Do(func() { close(g.gate) }) }

func (g *gatedRunner) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *gatedRunner) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the dump container")
	}
}

// versionQuerier answers SHOW server_version_num.
type versionQuerier struct{}

type versionRow struct{}

func (versionRow) Scan(dest ...any) error {
	p, ok := dest[0].(*string)
	if !ok || len(dest) != 1 {
		return fmt.Errorf("versionRow: unexpected destinations %T", dest)
	}
	*p = "170002"
	return nil
}

func (versionQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return versionRow{} }

type countingNotifier struct {
	mu sync.Mutex
	n  int
}

func (c *countingNotifier) StateBackupFailed(context.Context, string, string, string, string) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingNotifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

type stateFixture struct {
	svc    *statebackup.Service
	store  *statebackup.DBStore
	runner *gatedRunner
	notes  *countingNotifier
}

// newStateFixture builds a service on a real store whose settings are saved
// and enabled. The storage is unreachable, so a released run ends in error;
// what the tests look at happens while the run still holds its slot.
func newStateFixture(t *testing.T) *stateFixture {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f := &stateFixture{
		store:  statebackup.NewDBStore(db.New(testutil.NewTestDB(t))),
		runner: newGatedRunner(),
		notes:  &countingNotifier{},
	}
	f.svc = statebackup.New(versionQuerier{}, f.runner, f.store, "postgres://krill:pw@db.example:5432/krill?sslmode=disable", true)
	f.svc.SetNotifier(f.notes)
	ctx := context.Background()
	if err := f.store.Save(ctx, statebackup.Settings{
		Endpoint: "http://127.0.0.1:1", Bucket: "state", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk", HasSecretKey: true,
		Recipient: id.Recipient().String(), Schedule: "0 3 * * *", Retention: 14,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.runner.release()
		f.waitIdle(t)
	})
	return f
}

func (f *stateFixture) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for f.svc.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the run did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *stateFixture) runCount(t *testing.T) int {
	t.Helper()
	rs, err := f.store.ListRuns(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(rs)
}

// since returns what was logged after mark (a length of logs.String()).
func since(logs *syncBuffer, mark int) string { return logs.String()[mark:] }

func TestSnapshotHookMapsRunning(t *testing.T) {
	f := newStateFixture(t)
	if err := f.svc.Start(statebackup.TriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.runner.waitEntered(t)

	took, err := snapshotHook(f.svc)(context.Background(), "v1.0.0", "v1.0.1")
	var busy *selfupdate.BusyError
	if took || !errors.As(err, &busy) || busy.Reason != "state_backup" {
		t.Fatalf("hook while a run holds the slot = %v, %v; want a state_backup BusyError", took, err)
	}
}

func TestSnapshotHookPassesThrough(t *testing.T) {
	f := newStateFixture(t)
	if err := f.store.SetEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	took, err := snapshotHook(f.svc)(context.Background(), "v1.0.0", "v1.0.1")
	if took || err != nil {
		t.Fatalf("hook while paused = %v, %v; want false, nil", took, err)
	}
	if f.runner.callCount() != 0 {
		t.Fatalf("a paused state backup ran the dump")
	}
}

func TestScheduledStateRunSkipsDuringUpdate(t *testing.T) {
	f := newStateFixture(t)
	logs := captureLogs(t)
	var mu sync.Mutex
	updating := true
	f.svc.SetUpdateActive(func() bool { mu.Lock(); defer mu.Unlock(); return updating })
	run := scheduledStateRun(f.svc)

	run(context.Background(), 1)
	if n := f.svc.InFlight(); n != 0 {
		t.Fatalf("a run started during an update (InFlight = %d)", n)
	}
	if got := logs.String(); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "update is in progress") {
		t.Errorf("a run skipped for an update did not log at INFO: %q", got)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("a skipped run was journalled (%d rows)", n)
	}

	mu.Lock()
	updating = false
	mu.Unlock()
	run(context.Background(), 1)
	if n := f.svc.InFlight(); n != 1 {
		t.Fatalf("the scheduled run did not start (InFlight = %d)", n)
	}
	f.runner.waitEntered(t)

	mark := len(logs.String())
	run(context.Background(), 1) // the slot is taken
	if got := since(logs, mark); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "skipped") {
		t.Errorf("a scheduled run that found the slot taken did not log at INFO: %q", got)
	}
	if f.notes.count() != 0 || f.runCount(t) != 0 {
		t.Errorf("a skipped run alerted (%d) or was journalled (%d)", f.notes.count(), f.runCount(t))
	}
	if f.runner.callCount() != 1 {
		t.Errorf("dump calls = %d, want 1", f.runner.callCount())
	}

	f.runner.release()
	f.waitIdle(t)
	if n := f.runCount(t); n != 1 {
		t.Errorf("runs journalled = %d, want only the one that started", n)
	}
}

func (g *gatedRunner) removeCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.removed...)
}

func TestCleanupStateDumps(t *testing.T) {
	f := newStateFixture(t)
	logs := captureLogs(t)
	f.runner.n = 2
	cleanupStateDumps(context.Background(), f.svc)
	if got := f.runner.removeCalls(); len(got) != 1 || got[0] != "krill.role=state-dump" {
		t.Fatalf("removed = %v", got)
	}
	if !strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), "count=2") {
		t.Errorf("removed containers not logged at INFO: %q", logs.String())
	}

	mark := len(logs.String())
	f.runner.n, f.runner.rmErr = 0, errors.New("docker is slow")
	cleanupStateDumps(context.Background(), f.svc)
	if got := since(logs, mark); !strings.Contains(got, "level=WARN") {
		t.Errorf("a failed cleanup not logged at WARN: %q", got)
	}

	mark = len(logs.String())
	f.runner.rmErr = nil
	cleanupStateDumps(context.Background(), f.svc)
	if got := since(logs, mark); got != "" {
		t.Errorf("nothing removed, yet logged: %q", got)
	}

	// A run in flight keeps its container: the sweep is skipped, with INFO.
	if err := f.svc.Start(statebackup.TriggerManual); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.runner.waitEntered(t)
	calls := len(f.runner.removeCalls())
	mark = len(logs.String())
	cleanupStateDumps(context.Background(), f.svc)
	if len(f.runner.removeCalls()) != calls {
		t.Errorf("the sweep removed containers while a run was in flight")
	}
	if got := since(logs, mark); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "a run is in flight") {
		t.Errorf("a skipped sweep not logged at INFO: %q", got)
	}
}

func TestStateDigestAdapter(t *testing.T) {
	ctx := context.Background()
	store := statebackup.NewDBStore(db.New(testutil.NewTestDB(t)))
	a := stateDigestAdapter{store: store}

	st, err := a.StateBackupStatus(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Configured || st.Paused {
		t.Fatalf("nothing saved: %+v", st)
	}

	if err := store.Save(ctx, statebackup.Settings{
		Bucket: "state", AccessKey: "ak", SecretKey: "sk", HasSecretKey: true,
		Recipient: "age1x", Schedule: "0 3 * * *", Retention: 14,
	}); err != nil {
		t.Fatal(err)
	}
	view, err := store.LoadView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err = a.StateBackupStatus(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Configured || !st.Paused || st.Schedule != "0 3 * * *" || !st.Since.Equal(view.UpdatedAt) {
		t.Fatalf("saved but not enabled: %+v (updated_at %v)", st, view.UpdatedAt)
	}

	if err := store.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Microsecond)
	old := now.Add(-40 * 24 * time.Hour)
	for _, r := range []statebackup.Run{
		{Trigger: statebackup.TriggerSchedule, Status: "ok", SizeBytes: 10, StartedAt: old, FinishedAt: old},
		{Trigger: statebackup.TriggerPreUpdate, Status: "error", Error: "boom", FromVersion: "v1.0.0", ToVersion: "v1.0.1", StartedAt: now.Add(-2 * time.Hour), FinishedAt: now.Add(-2 * time.Hour)},
		{Trigger: statebackup.TriggerManual, Status: "ok", SizeBytes: 42, StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-time.Hour)},
	} {
		if err := store.RecordRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	st, err = a.StateBackupStatus(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Configured || st.Paused {
		t.Fatalf("enabled: %+v", st)
	}
	if !st.LastSuccess.Equal(now.Add(-time.Hour)) {
		t.Errorf("LastSuccess = %v, want %v", st.LastSuccess, now.Add(-time.Hour))
	}
	want := []notify.StateRun{
		{Trigger: "pre_update", Status: "error", Error: "boom", From: "v1.0.0", To: "v1.0.1", At: now.Add(-2 * time.Hour)},
		{Trigger: "manual", Status: "ok", Size: 42, At: now.Add(-time.Hour)},
	}
	if len(st.Runs) != len(want) {
		t.Fatalf("runs since = %+v", st.Runs)
	}
	for i, w := range want {
		g := st.Runs[i]
		if g.Trigger != w.Trigger || g.Status != w.Status || g.Error != w.Error || g.From != w.From || g.To != w.To || g.Size != w.Size || !g.At.Equal(w.At) {
			t.Errorf("run %d = %+v, want %+v", i, g, w)
		}
	}

	if err := a.PruneStateRuns(ctx, now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rs, err := store.ListRuns(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Errorf("after the prune %d runs remain, want 2", len(rs))
	}
}

// fakeReload fails its first fails calls, then succeeds; done is closed on
// the first success.
type fakeReload struct {
	mu    sync.Mutex
	calls int
	fails int
	done  chan struct{}
}

func (f *fakeReload) reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.fails {
		return errors.New("database unavailable")
	}
	if f.calls == f.fails+1 {
		close(f.done)
	}
	return nil
}

func (f *fakeReload) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func fastReloader(ctx context.Context, f *fakeReload) *schedReloader {
	r := newSchedReloader(ctx, f.reload)
	r.first, r.max = time.Millisecond, 5*time.Millisecond
	return r
}

// A failed reload is retried until it succeeds, so settings saved while the
// database blinked still take effect.
func TestSchedReloaderRetriesUntilSuccess(t *testing.T) {
	f := &fakeReload{fails: 3, done: make(chan struct{})}
	fastReloader(context.Background(), f).Reload()
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("reload never succeeded after %d calls", f.count())
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.count(); n != 4 {
		t.Fatalf("reload calls = %d, want 4 (retrying stops after the success)", n)
	}
}

// A successful reload needs no retry.
func TestSchedReloaderNoRetryOnSuccess(t *testing.T) {
	f := &fakeReload{done: make(chan struct{})}
	r := fastReloader(context.Background(), f)
	r.Reload()
	time.Sleep(50 * time.Millisecond)
	if n := f.count(); n != 1 {
		t.Fatalf("reload calls = %d, want 1", n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		t.Fatal("a retry is pending after a successful reload")
	}
}

// Retrying has no give-up bound: it outlasts any outage and ends only on
// success or shutdown.
func TestSchedReloaderOutlastsLongOutage(t *testing.T) {
	f := &fakeReload{fails: 200, done: make(chan struct{})}
	fastReloader(context.Background(), f).Reload()
	select {
	case <-f.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("reload never succeeded after %d calls", f.count())
	}
}

// Shutdown ends the retrying.
func TestSchedReloaderStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeReload{fails: 1 << 30, done: make(chan struct{})}
	fastReloader(ctx, f).Reload()
	for f.count() < 3 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	time.Sleep(100 * time.Millisecond) // let an in-flight attempt finish
	n := f.count()
	time.Sleep(100 * time.Millisecond)
	if f.count() != n {
		t.Fatal("reload is still retried after shutdown")
	}
}

// Failures while retrying are logged at most once per logGap.
func TestSchedReloaderThrottlesLogs(t *testing.T) {
	var buf syncBuf
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeReload{fails: 1 << 30, done: make(chan struct{})}
	r := fastReloader(ctx, f)
	r.logGap = time.Hour
	r.now = func() time.Time { return time.Unix(0, 0) } // the clock never moves
	r.Reload()
	for f.count() < 10 {
		time.Sleep(time.Millisecond)
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Fatalf("ERROR lines = %d, want 1 (only the first failure)", n)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
