package statebackup

import (
	"testing"
	"time"

	"github.com/proshik/krill/internal/backup"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func TestKeys(t *testing.T) {
	if got, want := ScheduledKey("ops", t0), "ops/krill-state/2026-11-20T10-00-00Z.sql.gz.age"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	loc := time.FixedZone("x", 3*3600)
	if got, want := ScheduledKey("ops", t0.In(loc)), "ops/krill-state/2026-11-20T10-00-00Z.sql.gz.age"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got, want := PreUpdateKey("ops", t0, "v0.9.3", "v0.9.4"), "ops/krill-state/pre-update/2026-11-20T10-00-00Z_v0.9.3_to_v0.9.4.sql.gz.age"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestKeysNormalizePrefix(t *testing.T) {
	for p, want := range map[string]string{"": "krill-state/", "/": "krill-state/", "/ops/": "ops/krill-state/", "ops//": "ops/krill-state/", "a//b": "a/b/krill-state/"} {
		if got := ScheduledDir(p); got != want {
			t.Errorf("%q: got %q want %q", p, got, want)
		}
		if got := PreUpdateDir(p); got != want+"pre-update/" {
			t.Errorf("%q: pre got %q", p, got)
		}
	}
}

func objs(keys ...string) []backup.Object {
	var o []backup.Object
	for i, k := range keys {
		o = append(o, backup.Object{Key: k, LastModified: t0.Add(time.Duration(-i) * time.Hour)})
	}
	return o
}

func keysOf(o []backup.Object) []string {
	var r []string
	for _, x := range o {
		r = append(r, x.Key)
	}
	return r
}

func sched(d int) string { return ScheduledKey("ops", t0.AddDate(0, 0, d)) }
func pre(d int) string   { return PreUpdateKey("ops", t0.AddDate(0, 0, d), "v1.0.0", "v1.0.1") }

func TestPrunableScheduled(t *testing.T) {
	in := objs(sched(3), sched(0), sched(2), sched(1), pre(0), pre(1),
		"ops/krill-state/notes.txt", "ops/krill-state/2026-11-20T10-00-00Z.sql.gz.age.bak")
	got := keysOf(Prunable(in, ScheduledDir("ops"), false, 2, ""))
	want := []string{sched(1), sched(0)}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPrunablePreUpdate(t *testing.T) {
	var in []backup.Object
	for i := 0; i < 7; i++ {
		in = append(in, backup.Object{Key: pre(i)})
	}
	in = append(in, backup.Object{Key: sched(0)}, backup.Object{Key: sched(1)})
	got := keysOf(Prunable(in, PreUpdateDir("ops"), true, PreUpdateKeep, ""))
	if len(got) != 2 || got[0] != pre(1) || got[1] != pre(0) {
		t.Fatalf("got %v", got)
	}
}

func TestPrunableOrdersByKeyNotModTime(t *testing.T) {
	in := []backup.Object{{Key: sched(0), LastModified: t0.Add(time.Hour)}, {Key: sched(1), LastModified: t0}}
	got := Prunable(in, ScheduledDir("ops"), false, 1, "")
	if len(got) != 1 || got[0].Key != sched(0) {
		t.Fatalf("got %v", got)
	}
}

func TestPrunableKeepFloor(t *testing.T) {
	got := Prunable(objs(sched(0), sched(1)), ScheduledDir("ops"), false, 0, "")
	if len(got) != 1 || got[0].Key != sched(0) {
		t.Fatalf("got %v", keysOf(got))
	}
}

// Only names the updater writes count: an operator's copy, or a name with a
// version the updater never runs with, is neither kept nor pruned.
func TestPrunablePreUpdateIgnoresCopies(t *testing.T) {
	dir := PreUpdateDir("ops")
	stamp := t0.UTC().Format(stampLayout)
	copies := []string{
		dir + stamp + "_v1.0.0_to_v1.2.4-copy.sql.gz.age",
		dir + stamp + "_v1.0.0-copy_to_v1.2.4.sql.gz.age",
		dir + stamp + "_v1.0.0_to_v1.2.sql.gz.age",
		dir + stamp + "_v1_to_v2.sql.gz.age",
		dir + stamp + "_v1.0.0_to_v1.2.4.copy.sql.gz.age",
		dir + stamp + "_v1.0.0_to_vX.Y.Z.sql.gz.age",
	}
	in := objs(copies...)
	for i := 0; i < PreUpdateKeep+1; i++ {
		in = append(in, backup.Object{Key: pre(-i - 1)})
	}
	got := keysOf(Prunable(in, dir, true, PreUpdateKeep, ""))
	if len(got) != 1 || got[0] != pre(-PreUpdateKeep-1) {
		t.Fatalf("got %v; want only the oldest generated dump", got)
	}
}

// The run's own object is never prunable and takes a keep slot, even when a
// clock set back sorts it behind later-dated keys.
func TestPrunableKeepsCurrent(t *testing.T) {
	cur := sched(-10)
	in := objs(sched(3), sched(2), sched(1), cur)
	got := keysOf(Prunable(in, ScheduledDir("ops"), false, 2, cur))
	if len(got) != 2 || got[0] != sched(2) || got[1] != sched(1) {
		t.Fatalf("got %v; want the two oldest later-dated keys, never the current one", got)
	}
	if got := Prunable(objs(sched(1), cur), ScheduledDir("ops"), false, 1, cur); len(got) != 1 || got[0].Key != sched(1) {
		t.Fatalf("keep 1: got %v", keysOf(got))
	}
}
