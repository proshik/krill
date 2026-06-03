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
	store SchedStore
	run   RunFunc
	mu    sync.Mutex
	cron  *cron.Cron
}

func NewScheduler(store SchedStore, run RunFunc) *Scheduler {
	return &Scheduler{store: store, run: run}
}

// Reload rebuilds the schedule from enabled backups; invalid cron exprs are skipped.
func (s *Scheduler) Reload() error {
	bs, err := s.store.ListEnabledBackups(context.Background())
	if err != nil {
		return err
	}
	c := cron.New()
	for _, b := range bs {
		id := b.ID
		if _, aerr := c.AddFunc(b.Schedule, func() { s.run(context.Background(), id) }); aerr != nil {
			slog.Warn("backup scheduler: invalid cron, skipping", "backup", id, "schedule", b.Schedule, "err", aerr)
		}
	}
	s.mu.Lock()
	old := s.cron
	s.cron = c
	s.mu.Unlock()
	c.Start()
	if old != nil {
		old.Stop()
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

// Stop halts the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		s.cron.Stop()
	}
}
