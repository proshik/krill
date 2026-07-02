package backup

import (
	"context"
	"errors"
	"testing"
	"time"
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
}

func (f *fakeBackupNotifier) BackupFailed(_ context.Context, id int64, _ string) {
	f.called = true
	f.backupID = id
}

func TestBackupFailureNotifies(t *testing.T) {
	st := &hookStore{}
	svc := New(nil, st)
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
