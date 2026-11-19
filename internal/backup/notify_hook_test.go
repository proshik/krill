package backup

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/proshik/krill/internal/testutil"
)

// hookStore is a minimal backup.Store: GetBackup succeeds, GetPGTarget fails so
// RunBackup funnels into fail().
type hookStore struct{ lastStatus string }

func (h *hookStore) GetBackup(_ context.Context, id int64) (BackupRow, error) {
	return BackupRow{ID: id, LogicalDatabaseID: 1, DestinationID: 1, Prefix: "", Retention: 7}, nil
}
func (h *hookStore) GetPGTarget(_ context.Context, _ int64) (PGTarget, error) {
	return PGTarget{}, errors.New("pg gone")
}
func (h *hookStore) GetDestination(_ context.Context, _ int64) (Destination, error) {
	return Destination{}, nil
}
func (h *hookStore) SetBackupResult(_ context.Context, _ int64, _ time.Time, status, _ string) error {
	h.lastStatus = status
	return nil
}

type fakeBackupNotifier struct {
	called   bool
	backupID int64
	okID     int64
	okSize   int64
}

func (f *fakeBackupNotifier) BackupFailed(_ context.Context, id int64, _ string) {
	f.called = true
	f.backupID = id
}

func (f *fakeBackupNotifier) BackupSucceeded(_ context.Context, id, size int64) {
	f.okID, f.okSize = id, size
}

func TestBackupFailureNotifies(t *testing.T) {
	st := &hookStore{}
	svc := New(nil, st, false)
	fn := &fakeBackupNotifier{}
	svc.SetNotifier(fn)

	err := svc.RunBackup(context.Background(), 42, time.Unix(0, 0))
	if err == nil {
		t.Fatal("expected error")
	}
	if st.lastStatus != "error" {
		t.Fatalf("status = %q, want error", st.lastStatus)
	}
	if !fn.called || fn.backupID != 42 {
		t.Fatalf("notifier not called for backup 42: %+v", fn)
	}
}

// minioStore is a backup.Store pointing at a real MinIO destination.
type minioStore struct {
	leakStore
	dst        Destination
	lastStatus string
}

func (m *minioStore) GetDestination(context.Context, int64) (Destination, error) { return m.dst, nil }
func (m *minioStore) SetBackupResult(_ context.Context, _ int64, _ time.Time, status, _ string) error {
	m.lastStatus = status
	return nil
}

// dumpExecer writes a fixed "pg_dump" output.
type dumpExecer struct{}

func (dumpExecer) Exec(_ context.Context, _ string, _ []string, _ []string, _ io.Reader, stdout io.Writer) error {
	_, err := stdout.Write([]byte("-- dump\nCREATE TABLE t (id int);\n"))
	return err
}

// A successful run tells the notifier the size of what was stored — the
// compressed dump — for the digest's backup journal.
func TestBackupSuccessReportsStoredSize(t *testing.T) {
	m := testutil.NewMinio(t)
	dst := Destination{Endpoint: m.Endpoint, Bucket: "test", Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
	ctx := context.Background()
	if err := CreateBucket(ctx, dst, true); err != nil {
		t.Fatal(err)
	}
	st := &minioStore{dst: dst}
	svc := New(dumpExecer{}, st, true)
	fn := &fakeBackupNotifier{}
	svc.SetNotifier(fn)

	if err := svc.RunBackup(ctx, 5, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	objs, err := List(ctx, dst, "", true)
	if err != nil || len(objs) != 1 {
		t.Fatalf("stored objects = %+v, %v", objs, err)
	}
	if st.lastStatus != "ok" || fn.okID != 5 || fn.okSize != objs[0].Size || fn.okSize == 0 {
		t.Fatalf("status %q, BackupSucceeded(%d, %d); want (5, %d)", st.lastStatus, fn.okID, fn.okSize, objs[0].Size)
	}
	if fn.called {
		t.Fatal("a successful run also reported a failure")
	}
}
