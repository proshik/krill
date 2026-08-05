package backup

import (
	"context"
	"log/slog"
	"sync"

	"github.com/robfig/cron/v3"
)

// SchedBackup is the minimal info the scheduler needs.
type SchedBackup struct {
	ID       int64
	Schedule string
}

// SchedStore lists the enabled backups to schedule.
type SchedStore interface {
	ListEnabledBackups(ctx context.Context) ([]SchedBackup, error)
}

// RunFunc runs a backup by id.
type RunFunc func(ctx context.Context, backupID int64)

// Scheduler runs enabled backups on their cron schedules.
type Scheduler struct {
	store   SchedStore
	run     RunFunc
	mu      sync.Mutex
	cron    *cron.Cron
	stopped bool // set by Stop: a later Reload must not resurrect the scheduler
}

func NewScheduler(store SchedStore, run RunFunc) *Scheduler {
	return &Scheduler{store: store, run: run}
}

// Reload rebuilds the schedule from enabled backups; empty (on-demand) schedules and invalid cron exprs are skipped.
func (s *Scheduler) Reload() error {
	bs, err := s.store.ListEnabledBackups(context.Background())
	if err != nil {
		return err
	}
	// SkipIfStillRunning ensures a slow backup never overlaps its next scheduled run.
	c := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)))
	for _, b := range bs {
		if b.Schedule == "" {
			continue // on-demand backup: no automatic runs
		}
		id := b.ID
		if _, aerr := c.AddFunc(b.Schedule, func() { s.run(context.Background(), id) }); aerr != nil {
			slog.Warn("backup scheduler: invalid cron, skipping", "backup", id, "schedule", b.Schedule, "err", aerr)
		}
	}
	// Swap, start and stop under one lock. Starting outside it let two
	// concurrent Reloads Stop a cron that had not been Started yet and then
	// Start it anyway, leaving a running instance nobody references — duplicate
	// backup runs that Stop could no longer reach.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil // shutting down: never start a cron that would outlive Stop
	}
	old := s.cron
	s.cron = c
	c.Start()
	if old != nil {
		old.Stop() // non-blocking: signals the scheduler goroutine and returns
	}
	return nil
}

func (s *Scheduler) entryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return 0
	}
	return len(s.cron.Entries())
}

// Stop halts the scheduler permanently: a Reload racing shutdown (a backup
// mutation arriving as the process exits) must not start a fresh cron.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.cron != nil {
		s.cron.Stop()
	}
}
