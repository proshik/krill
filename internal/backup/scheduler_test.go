package backup

import (
	"context"
	"testing"
)

type fakeSchedStore struct{ backups []SchedBackup }

func (f *fakeSchedStore) ListEnabledBackups(ctx context.Context) ([]SchedBackup, error) {
	return f.backups, nil
}

func TestSchedulerReload(t *testing.T) {
	st := &fakeSchedStore{backups: []SchedBackup{{ID: 1, Schedule: "0 3 * * *"}, {ID: 2, Schedule: "@daily"}}}
	sc := NewScheduler(st, func(ctx context.Context, id int64) {})
	if err := sc.Reload(); err != nil {
		t.Fatal(err)
	}
	defer sc.Stop()
	if n := sc.entryCount(); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}
	st.backups = append(st.backups, SchedBackup{ID: 3, Schedule: "not a cron"})
	if err := sc.Reload(); err != nil {
		t.Fatal(err)
	}
	if n := sc.entryCount(); n != 2 {
		t.Fatalf("entries = %d, want 2 (bad cron skipped)", n)
	}
}

func TestSchedulerSkipsEmptySchedule(t *testing.T) {
	st := &fakeSchedStore{backups: []SchedBackup{
		{ID: 1, Schedule: "0 3 * * *"}, // scheduled
		{ID: 2, Schedule: ""},          // on-demand: must be skipped
	}}
	s := NewScheduler(st, func(context.Context, int64) {})
	if err := s.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer s.Stop()
	if n := s.entryCount(); n != 1 {
		t.Fatalf("want 1 cron entry (empty schedule skipped), got %d", n)
	}
}
