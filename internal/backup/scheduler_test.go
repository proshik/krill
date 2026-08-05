package backup

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeSchedStore struct{ backups []SchedBackup }

func (f *fakeSchedStore) ListEnabledBackups(ctx context.Context) ([]SchedBackup, error) {
	return f.backups, nil
}

// Reload used to swap the new cron in under the lock but call Start on it (and
// Stop on the old one) outside it. Two Reloads could interleave so the loser's
// cron was Stopped before it was Started, then Started anyway: a running cron
// nobody references, firing every backup twice, unreachable by Stop.
//
// The window is nanoseconds wide and this test does not reliably reproduce it
// (it did not fire even under -race); it stands as a regression guard on the
// invariant that survives the fix — after Stop, nothing runs. The reachable
// half of the same defect is covered deterministically by
// TestReloadAfterStopDoesNotStart.
func TestConcurrentReloadDoesNotLeakRunningCron(t *testing.T) {
	st := &fakeSchedStore{backups: []SchedBackup{{ID: 1, Schedule: "@every 100ms"}}}
	var mu sync.Mutex
	runs := 0
	s := NewScheduler(st, func(context.Context, int64) {
		mu.Lock()
		runs++
		mu.Unlock()
	})

	// Many rounds of concurrent Reloads: the losing swap has to land between a
	// winner's swap and its Start, which is a narrow window worth hammering.
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Reload(); err != nil {
					t.Errorf("reload: %v", err)
				}
			}()
		}
		wg.Wait()
	}

	time.Sleep(350 * time.Millisecond) // let the live scheduler fire a few times
	s.Stop()
	time.Sleep(50 * time.Millisecond) // let an in-flight tick settle
	mu.Lock()
	atStop := runs
	mu.Unlock()

	time.Sleep(400 * time.Millisecond) // nothing may fire after Stop
	mu.Lock()
	after := runs
	mu.Unlock()

	if after != atStop {
		t.Errorf("cron kept firing after Stop (%d -> %d): a concurrent Reload leaked a running instance", atStop, after)
	}
}

// Reload after Stop must not resurrect the scheduler: shutdown calls Stop, and
// any Reload still in flight (a backup mutation racing the shutdown) would
// otherwise start a fresh cron that outlives it.
func TestReloadAfterStopDoesNotStart(t *testing.T) {
	st := &fakeSchedStore{backups: []SchedBackup{{ID: 1, Schedule: "@every 100ms"}}}
	var mu sync.Mutex
	runs := 0
	s := NewScheduler(st, func(context.Context, int64) {
		mu.Lock()
		runs++
		mu.Unlock()
	})

	s.Stop()
	if err := s.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	time.Sleep(350 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if runs != 0 {
		t.Errorf("scheduler ran %d times after Stop", runs)
	}
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
