package backup

import (
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
