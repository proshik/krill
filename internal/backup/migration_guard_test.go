package backup

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/oplock"
)

// migratingStore is a backup.Store whose PGTarget always resolves to a fixed
// AppName. Tests pre-lock that instance's oplock (as the migration job would)
// and assert the migration-refusal guard fires before any dump/restore work
// starts. GetDestination panics if reached — the guard must trip earlier.
type migratingStore struct {
	appName string
}

func (m *migratingStore) GetBackup(_ context.Context, id int64) (BackupRow, error) {
	return BackupRow{ID: id, LogicalDatabaseID: 1, DestinationID: 1, Prefix: "", Retention: 7}, nil
}
func (m *migratingStore) GetPGTarget(_ context.Context, _ int64) (PGTarget, error) {
	return PGTarget{AppName: m.appName, DatabaseName: "db", DatabaseUser: "u", DatabasePassword: "p"}, nil
}
func (m *migratingStore) GetDestination(_ context.Context, _ int64) (Destination, error) {
	panic("GetDestination must not be called while the instance is migrating")
}
func (m *migratingStore) SetBackupResult(_ context.Context, _ int64, _ time.Time, _, _ string) error {
	return nil
}

// noopExecer records whether it was ever invoked — used to prove the
// migration guard short-circuits before any dump/restore Exec call.
type noopExecer struct{ called bool }

func (n *noopExecer) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	n.called = true
	return nil
}

// TestRunBackupRefusedDuringMigration: with the target instance's oplock held
// (as the volume-migration job holds it for the duration of a migration),
// RunBackup must refuse with a "migrating" error and never start the dump.
func TestRunBackupRefusedDuringMigration(t *testing.T) {
	const appName = "krill-pg-m"
	lock := oplock.DBInstance(appName)
	if !oplock.TryAcquire(lock) {
		t.Fatal("setup: could not acquire lock")
	}
	defer oplock.Release(lock)

	ex := &noopExecer{}
	svc := New(ex, &migratingStore{appName: appName}, false)

	err := svc.RunBackup(context.Background(), 1, time.Unix(0, 0))
	if err == nil || !strings.Contains(err.Error(), "migrating") {
		t.Fatalf("want migrating refusal, got %v", err)
	}
	if ex.called {
		t.Fatal("dump must not start while the instance is migrating")
	}
}

// TestRestoreByIDRefusedDuringMigration: same guard on the restore path.
func TestRestoreByIDRefusedDuringMigration(t *testing.T) {
	const appName = "krill-pg-m2"
	lock := oplock.DBInstance(appName)
	if !oplock.TryAcquire(lock) {
		t.Fatal("setup: could not acquire lock")
	}
	defer oplock.Release(lock)

	ex := &noopExecer{}
	svc := New(ex, &migratingStore{appName: appName}, false)

	err := svc.RestoreByID(context.Background(), 1, "whatever/key.sql.gz")
	if err == nil || !strings.Contains(err.Error(), "migrating") {
		t.Fatalf("want migrating refusal, got %v", err)
	}
	if ex.called {
		t.Fatal("restore must not start while the instance is migrating")
	}
}
