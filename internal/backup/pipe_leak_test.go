package backup

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// leakStore is a backup.Store whose destination points at an unreachable
// endpoint, so Upload fails fast while the dump goroutine is mid-write.
type leakStore struct{}

func (leakStore) GetBackup(_ context.Context, id int64) (BackupRow, error) {
	return BackupRow{ID: id, LogicalDatabaseID: 1, DestinationID: 1, Prefix: "", Retention: 7}, nil
}
func (leakStore) GetPGTarget(_ context.Context, _ int64) (PGTarget, error) {
	return PGTarget{AppName: "db", DatabaseName: "db", DatabaseUser: "u", DatabasePassword: "p"}, nil
}
func (leakStore) GetDestination(_ context.Context, _ int64) (Destination, error) {
	// Nothing listens on port 1 — Upload errors with connection refused.
	return Destination{Endpoint: "http://127.0.0.1:1", Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s"}, nil
}
func (leakStore) SetBackupResult(_ context.Context, _ int64, _ time.Time, _, _ string) error {
	return nil
}

// blockingExecer simulates pg_dump: it keeps writing output until the writer
// errors (pipe closed), then returns and signals done (once — Exec may be
// called again by tests that run several backups).
type blockingExecer struct {
	done chan struct{}
	once sync.Once
}

func (b *blockingExecer) Exec(_ context.Context, _ string, _ []string, _ []string, _ io.Reader, stdout io.Writer) error {
	defer b.once.Do(func() { close(b.done) })
	// Incompressible (pseudo-random, fixed seed) data: the S3 uploader buffers a
	// full 5MB part from the gzip stream before dialing, and compressible zeros
	// would force gigabytes of raw writes through gzip to fill it.
	chunk := make([]byte, 32<<10)
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(chunk)
	for {
		if _, err := stdout.Write(chunk); err != nil {
			return err
		}
	}
}

// failExecer aborts the dump immediately — for tests where only RunBackup's
// control flow matters, keeping them independent of S3-uploader timing.
type failExecer struct{}

func (failExecer) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return errors.New("dump aborted")
}

// gateStore parks RunBackup inside GetDestination until the gate opens,
// signalling `entered` once — used to hold a run "in flight" deterministically.
type gateStore struct {
	leakStore
	entered chan struct{}
	gate    chan struct{}
}

func (g *gateStore) GetDestination(ctx context.Context, id int64) (Destination, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.gate
	return g.leakStore.GetDestination(ctx, id)
}

// TestRunBackupSkipsOverlap locks in the per-ID overlap guard: while a run of
// backup 7 is in flight, a second trigger of the SAME backup must be rejected
// with ErrBackupRunning (cron SkipIfStillRunning does not survive
// Scheduler.Reload and never covered the manual path), and the guard must
// clear once the run finishes.
func TestRunBackupSkipsOverlap(t *testing.T) {
	gs := &gateStore{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	// The in-flight window is held by the gate (inside GetDestination), so the
	// dump itself can abort instantly — no dependence on uploader timing.
	svc := New(failExecer{}, gs)

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errCh <- svc.RunBackup(ctx, 7, time.Unix(0, 0))
	}()
	select {
	case <-gs.entered: // run #1 is in flight, parked on the gate
	case <-time.After(2 * time.Second):
		t.Fatal("first run never started")
	}

	if err := svc.RunBackup(context.Background(), 7, time.Unix(0, 0)); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("overlapping run: want ErrBackupRunning, got %v", err)
	}

	close(gs.gate) // let run #1 proceed (it fails on the dead endpoint)
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("first run did not finish")
	}

	// Guard must be released: a fresh run is accepted again (and fails on the
	// dead endpoint, NOT with ErrBackupRunning).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.RunBackup(ctx, 7, time.Unix(0, 0)); errors.Is(err, ErrBackupRunning) {
		t.Fatalf("guard not released after the run finished: %v", err)
	}
}

// TestRunBackupUploadFailureReleasesDump locks in the fix for the pipe leak:
// when Upload fails, RunBackup must close the pipe reader so the dump
// goroutine's blocked write fails and the goroutine (in production: the docker
// exec + in-container pg_dump) terminates instead of leaking forever.
func TestRunBackupUploadFailureReleasesDump(t *testing.T) {
	ex := &blockingExecer{done: make(chan struct{})}
	svc := New(ex, leakStore{})

	// Short deadline so the SDK gives up on the dead endpoint quickly instead
	// of burning its full retry budget (~15s).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.RunBackup(ctx, 1, time.Unix(0, 0)); err == nil {
		t.Fatal("expected upload error")
	}

	select {
	case <-ex.done:
		// dump goroutine exited — pipe was closed on the error path
	case <-time.After(5 * time.Second):
		t.Fatal("dump goroutine still running 5s after Upload failed — pipe reader was not closed (leak)")
	}
}
