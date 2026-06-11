package backup

import (
	"context"
	"io"
	"testing"
	"time"
)

// leakStore is a backup.Store whose destination points at an unreachable
// endpoint, so Upload fails fast while the dump goroutine is mid-write.
type leakStore struct{}

func (leakStore) GetBackup(_ context.Context, id int64) (BackupRow, error) {
	return BackupRow{ID: id, PostgresDbID: 1, DestinationID: 1, Prefix: "", Retention: 7}, nil
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
// errors (pipe closed), then returns and signals done.
type blockingExecer struct{ done chan struct{} }

func (b *blockingExecer) Exec(_ context.Context, _ string, _ []string, _ []string, _ io.Reader, stdout io.Writer) error {
	defer close(b.done)
	chunk := make([]byte, 32<<10)
	for {
		if _, err := stdout.Write(chunk); err != nil {
			return err
		}
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
