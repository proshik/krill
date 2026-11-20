package statebackup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

func sample() Settings {
	return Settings{
		Endpoint: "https://s3.a.example", Bucket: "b", Region: "r", AccessKey: "ak",
		SecretKey: "s3cret", Prefix: "p", Recipient: "age1abc", Schedule: "0 3 * * *", Retention: 7,
	}
}

func TestStoreRoundTrip(t *testing.T) {
	secret.Init("test-key")
	defer secret.Init("")
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	st := NewDBStore(q)
	ctx := context.Background()

	if err := st.Save(ctx, sample()); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := sample()
	if got.SecretKey != "s3cret" || !got.HasSecretKey || got.Bucket != want.Bucket ||
		got.Retention != 7 || got.Schedule != want.Schedule || got.UpdatedAt.IsZero() {
		t.Errorf("Load = %+v", got)
	}
	row, err := q.GetStateBackupSettings(ctx)
	if err != nil || !strings.HasPrefix(row.SecretKey, "enc:v1:") {
		t.Errorf("raw column = %q, %v", row.SecretKey, err)
	}
	v, err := st.LoadView(ctx)
	if err != nil || v.SecretKey != "" || !v.HasSecretKey {
		t.Errorf("LoadView = %+v, %v", v, err)
	}

	// Saving with the key left empty (kept) preserves the stored value.
	keep := sample()
	keep.SecretKey, keep.HasSecretKey, keep.Bucket = "", true, "b2"
	if err := st.Save(ctx, keep); err != nil {
		t.Fatal(err)
	}
	got, err = st.Load(ctx)
	if err != nil || got.SecretKey != "s3cret" || got.Bucket != "b2" {
		t.Errorf("after keep: %+v, %v", got, err)
	}
}

func TestStoreLoadUnset(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := NewDBStore(db.New(pool))
	for name, f := range map[string]func(context.Context) (Settings, error){"Load": st.Load, "LoadView": st.LoadView} {
		got, err := f(context.Background())
		if err != nil || got.Retention != 14 || got.Enabled || got.Bucket != "" || got.HasSecretKey {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	if err := st.SetEnabled(context.Background(), true); err == nil {
		t.Error("SetEnabled on an unsaved row must fail")
	}
}

func TestStoreUndecryptable(t *testing.T) {
	secret.Init("old-key")
	enc := secret.Enc("s3cret")
	secret.Init("new-key")
	defer secret.Init("")
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	if err := q.UpsertStateBackupSettings(context.Background(), db.UpsertStateBackupSettingsParams{
		SecretKey: enc, Retention: 14,
	}); err != nil {
		t.Fatal(err)
	}
	st := NewDBStore(q)
	if _, err := st.Load(context.Background()); !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("Load err = %v", err)
	}
	v, err := st.LoadView(context.Background())
	if err != nil || !v.HasSecretKey {
		t.Errorf("LoadView = %+v, %v", v, err)
	}
}

func TestStoreEnabledCheck(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := NewDBStore(db.New(pool))
	ctx := context.Background()
	s := sample()
	s.Recipient = ""
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabled(ctx, true); err == nil {
		t.Fatal("enabled without a recipient must fail")
	}
	s.Recipient = "age1abc"
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	s.Recipient = ""
	if err := st.Save(ctx, s); err == nil {
		t.Fatal("clearing the recipient of an enabled backup must fail")
	}
	if err := st.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LoadView(ctx); got.Enabled {
		t.Error("still enabled")
	}
}

// Only SetEnabled changes the flag: a save carrying a stale Enabled (read
// before a concurrent pause or resume) must not undo it.
func TestStoreSaveNeverChangesEnabled(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := NewDBStore(db.New(pool))
	ctx := context.Background()
	s := sample()
	s.Enabled = true
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LoadView(ctx); got.Enabled {
		t.Fatal("a first save must start paused")
	}
	if err := st.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	s.Enabled = false // read before the resume
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LoadView(ctx); !got.Enabled {
		t.Fatal("a stale save undid the resume")
	}
	if err := st.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	s.Enabled = true // read before the pause
	if err := st.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LoadView(ctx); got.Enabled {
		t.Fatal("a stale save undid the pause")
	}
}

func TestStoreRuns(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := NewDBStore(db.New(pool))
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	runs := []Run{
		{Trigger: TriggerSchedule, Status: "ok", ObjectKey: "k1", SizeBytes: 10, StartedAt: base.Add(-3 * time.Hour), FinishedAt: base.Add(-3 * time.Hour)},
		{Trigger: TriggerManual, Status: "ok", ObjectKey: "k2", SizeBytes: 20, StartedAt: base.Add(-2 * time.Hour), FinishedAt: base.Add(-2 * time.Hour)},
		{Trigger: TriggerPreUpdate, Status: "error", FromVersion: "v1", ToVersion: "v2", Error: "boom", StartedAt: base.Add(-time.Hour), FinishedAt: base.Add(-time.Hour)},
	}
	for _, r := range runs {
		if err := st.RecordRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	l, err := st.ListRuns(ctx, 2)
	if err != nil || len(l) != 2 || l[0].Trigger != TriggerPreUpdate || l[1].ObjectKey != "k2" || l[0].Error != "boom" {
		t.Fatalf("ListRuns = %+v, %v", l, err)
	}
	s, err := st.RunsSince(ctx, base.Add(-150*time.Minute))
	if err != nil || len(s) != 2 || s[0].ObjectKey != "k2" || s[1].Status != "error" {
		t.Fatalf("RunsSince = %+v, %v", s, err)
	}
	last, ok, err := st.LastSuccess(ctx)
	if err != nil || !ok || last.ObjectKey != "k2" || last.SizeBytes != 20 {
		t.Fatalf("LastSuccess = %+v, %v, %v", last, ok, err)
	}
	if err := st.PruneRuns(ctx, base.Add(-150*time.Minute)); err != nil {
		t.Fatal(err)
	}
	l, _ = st.ListRuns(ctx, 10)
	if len(l) != 2 {
		t.Fatalf("after prune: %+v", l)
	}
	// Pruning everything still keeps the newest success: the page and the
	// digest read the last success from the journal.
	if err := st.PruneRuns(ctx, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if l, _ = st.ListRuns(ctx, 10); len(l) != 1 || l[0].ObjectKey != "k2" {
		t.Fatalf("after pruning everything: %+v", l)
	}
	if last, ok, err := st.LastSuccess(ctx); !ok || err != nil || last.ObjectKey != "k2" {
		t.Errorf("LastSuccess after pruning everything = %+v, %v, %v", last, ok, err)
	}
}

// With no successful run at all, pruning removes every old row.
func TestStorePruneRunsWithoutSuccess(t *testing.T) {
	st := NewDBStore(db.New(testutil.NewTestDB(t)))
	ctx := context.Background()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for range 2 {
		if err := st.RecordRun(ctx, Run{Trigger: TriggerSchedule, Status: "error", Error: "boom", StartedAt: old, FinishedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PruneRuns(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if l, err := st.ListRuns(ctx, 10); err != nil || len(l) != 0 {
		t.Fatalf("after prune: %+v, %v", l, err)
	}
	if _, ok, err := st.LastSuccess(ctx); ok || err != nil {
		t.Errorf("LastSuccess on empty = %v, %v", ok, err)
	}
}
