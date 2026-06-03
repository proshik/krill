package backup

import (
	"testing"
	"time"
)

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
	if got := keyFor("p", "krill-postgres-x", ts); got != "p/krill-postgres-x/2026-06-03T12-30-00Z.sql.gz" {
		t.Fatalf("keyFor = %q", got)
	}
	if got := keyFor("", "app", ts); got != "app/2026-06-03T12-30-00Z.sql.gz" {
		t.Fatalf("keyFor empty prefix = %q", got)
	}
}
