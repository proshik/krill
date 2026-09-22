package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A retention pass that fails after a successful upload used to be logged and
// forgotten: the run was recorded "ok" with no message, so the bucket grew
// without bound and the only symptom was the S3 bill. The dump really is stored,
// so the run stays "ok" — but the note must reach last_error, which the UI
// renders under the status.
func TestRetentionNote(t *testing.T) {
	if got := retentionNote(nil, 0); got != "" {
		t.Errorf("clean pass: got %q, want empty", got)
	}
	if got := retentionNote(errors.New("connection refused"), 0); got == "" {
		t.Error("list failure produced no note")
	} else if !strings.Contains(got, "connection refused") {
		t.Errorf("list failure note drops the cause: %q", got)
	}
	if got := retentionNote(nil, 3); got == "" {
		t.Error("delete failures produced no note")
	} else if !strings.Contains(got, "3") {
		t.Errorf("delete-failure note drops the count: %q", got)
	}
	// A list failure means no deletes were attempted; report the root cause.
	if got := retentionNote(errors.New("boom"), 2); !strings.Contains(got, "boom") {
		t.Errorf("combined note drops the list cause: %q", got)
	}
}

func TestObjectsToDelete(t *testing.T) {
	now := time.Now()
	objs := []Object{
		{Key: "d/4", LastModified: now},
		{Key: "d/3", LastModified: now.Add(-time.Hour)},
		{Key: "d/2", LastModified: now.Add(-2 * time.Hour)},
		{Key: "d/1", LastModified: now.Add(-3 * time.Hour)},
	}
	del := objectsToDelete(objs, 2)
	if len(del) != 2 || del[0].Key != "d/2" || del[1].Key != "d/1" {
		t.Fatalf("del = %+v", del)
	}
	if len(objectsToDelete(objs, 10)) != 0 {
		t.Error("keep >= len -> delete nothing")
	}
	if len(objectsToDelete(objs, 0)) != 3 {
		t.Error("keep<1 treated as 1 -> delete all but newest")
	}
}

func TestKeyFor(t *testing.T) {
	ts := time.Date(2026, 6, 3, 12, 30, 0, 0, time.UTC)
	if got := keyFor("p", "krill-postgres-x", "app", ts); got != "p/krill-postgres-x/app/2026-06-03T12-30-00Z.sql.gz" {
		t.Fatalf("keyFor = %q", got)
	}
	if got := keyFor("", "app", "db", ts); got != "app/db/2026-06-03T12-30-00Z.sql.gz" {
		t.Fatalf("keyFor empty prefix = %q", got)
	}
}

func TestKeyForPerDatabase(t *testing.T) {
	now := time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC)
	if got := keyFor("krill", "pg-inst", "shop", now); got != "krill/pg-inst/shop/2026-07-02T03-00-00Z.sql.gz" {
		t.Fatalf("keyFor: %s", got)
	}
	if got := prefixDir("", "pg-inst", "shop"); got != "pg-inst/shop/" {
		t.Fatalf("prefixDir: %s", got)
	}
}

// InFlight lets the self-updater see whether a backup or restore is running
// anywhere right now, so it can refuse to restart Krill mid-run.
func TestInFlight(t *testing.T) {
	svc := New(failExecer{}, leakStore{}, false)
	if got := svc.InFlight(); got != 0 {
		t.Fatalf("InFlight on a fresh service = %d, want 0", got)
	}

	gs := &gateStore{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	blocked := New(failExecer{}, gs, false)

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errCh <- blocked.RunBackup(ctx, 7, time.Unix(0, 0))
	}()
	select {
	case <-gs.entered: // run is in flight, parked on the gate
	case <-time.After(2 * time.Second):
		t.Fatal("backup never started")
	}

	if got := blocked.InFlight(); got != 1 {
		t.Fatalf("InFlight while a backup runs = %d, want 1", got)
	}

	close(gs.gate) // let it proceed (fails on the dead endpoint)
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("backup did not finish")
	}

	if got := blocked.InFlight(); got != 0 {
		t.Fatalf("InFlight after the backup finished = %d, want 0", got)
	}
}

// A "Backup now" or restore that loses to a running one is refused to its
// caller, instead of being reported as started and then dropped.
func TestStartRefusesWhileRunning(t *testing.T) {
	svc := New(failExecer{}, leakStore{}, false)
	release, ok := svc.claim(1)
	if !ok {
		t.Fatal("setup: claim")
	}
	defer release()
	if err := svc.StartBackup(1, time.Now(), time.Minute); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("StartBackup: want ErrBackupRunning, got %v", err)
	}
	if err := svc.StartRestore(1, "k", time.Minute); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("StartRestore: want ErrBackupRunning, got %v", err)
	}
	if n := svc.InFlight(); n != 1 {
		t.Fatalf("a refused start must not claim anything, in flight = %d", n)
	}
}
