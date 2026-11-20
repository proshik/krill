package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/statebackup"
)

// stateDumpCleanupTimeout bounds the startup removal of leftover dump
// containers; it runs in the background, so a slow Docker never holds boot.
const stateDumpCleanupTimeout = 30 * time.Second

// Reload retries after a failed state backup schedule reload: the first
// waits reloadRetryFirst, each next one twice as long up to reloadRetryMax,
// and retrying stops after reloadRetryFor.
const (
	reloadRetryFirst = 2 * time.Second
	reloadRetryMax   = time.Minute
	reloadRetryFor   = time.Hour
)

// schedReloader reloads the state backup schedule after its settings change
// and, when the reload fails (it reads the settings from the database), keeps
// retrying in the background: Save and SetEnabled have already stored the new
// settings, which would otherwise stay out of effect until the next change or
// restart. A later reload supersedes a pending retry; ctx (the process's)
// ends it on shutdown.
type schedReloader struct {
	ctx    context.Context
	reload func() error
	first  time.Duration
	max    time.Duration
	giveUp time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc // the pending retry, if any
}

func newSchedReloader(ctx context.Context, reload func() error) *schedReloader {
	return &schedReloader{ctx: ctx, reload: reload, first: reloadRetryFirst, max: reloadRetryMax, giveUp: reloadRetryFor}
}

// Reload reloads the schedule now and, on failure, starts retrying.
func (r *schedReloader) Reload() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.mu.Unlock()
	err := r.reload()
	if err == nil {
		return
	}
	slog.Error("state backup scheduler reload failed; retrying in the background", "err", err)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil { // a concurrent Reload failed too: one retry is enough
		r.cancel()
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.giveUp)
	r.cancel = cancel
	go r.retry(ctx, cancel)
}

func (r *schedReloader) retry(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	wait := r.first
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.ctx.Err() == nil {
				slog.Error("state backup scheduler reload: giving up; save the settings again to retry", "after", r.giveUp)
			}
			return
		case <-t.C:
		}
		if err := r.reload(); err != nil {
			slog.Error("state backup scheduler reload failed again", "err", err)
		} else {
			slog.Info("state backup scheduler reloaded after an earlier failure")
			return
		}
		wait = min(2*wait, r.max)
	}
}

// snapshotHook is the self-updater's pre-update snapshot. A run that already
// holds the state backup's slot refuses the update as busy instead of
// waiting for it.
func snapshotHook(svc *statebackup.Service) selfupdate.SnapshotFunc {
	return func(ctx context.Context, from, to string) (bool, error) {
		took, err := svc.Snapshot(ctx, from, to)
		if errors.Is(err, statebackup.ErrStateBackupRunning) {
			return false, &selfupdate.BusyError{Reason: "state_backup"}
		}
		return took, err
	}
}

// scheduledStateRun is the state backup scheduler's run function. A run is
// skipped while a self-update job is active (that job's own pre-update
// snapshot covers the moment) and when it finds the slot taken; neither is
// alerted or journalled.
func scheduledStateRun(svc *statebackup.Service) backup.RunFunc {
	return func(context.Context, int64) {
		switch err := svc.Start(statebackup.TriggerSchedule); {
		case err == nil:
		case errors.Is(err, statebackup.ErrUpdateInProgress):
			slog.Info("scheduled state backup skipped: an update is in progress")
		case errors.Is(err, statebackup.ErrStateBackupRunning):
			slog.Info("scheduled state backup skipped: another state backup is still running")
		case errors.Is(err, statebackup.ErrNotConfigured):
			slog.Info("scheduled state backup skipped: Krill backups are not set up or are paused")
		default:
			slog.Error("scheduled state backup could not start", "err", err)
		}
	}
}

// cleanupStateDumps removes dump containers a crashed run left behind: a
// running one's pg_dump still holds the database password in its process
// environment. The service holds its run
// slot while it sweeps, so a run in flight keeps its container (it logs the
// skip itself).
func cleanupStateDumps(ctx context.Context, svc *statebackup.Service) {
	ctx, cancel := context.WithTimeout(ctx, stateDumpCleanupTimeout)
	defer cancel()
	n, err := svc.RemoveLeftovers(ctx)
	if n > 0 {
		slog.Info("removed leftover state backup containers", "count", n)
	}
	if err != nil {
		slog.Warn("could not remove leftover state backup containers", "err", err)
	}
}

// stateDigestAdapter is the digest's view of the state backup.
type stateDigestAdapter struct{ store statebackup.Store }

var _ notify.StateReader = stateDigestAdapter{}

// StateBackupStatus reports the settings and the runs finished since t.
// Configured means storage and a recipient are saved; Paused means they are
// but the state backup is turned off.
func (a stateDigestAdapter) StateBackupStatus(ctx context.Context, since time.Time) (notify.StateStatus, error) {
	v, err := a.store.LoadView(ctx)
	if err != nil {
		return notify.StateStatus{}, err
	}
	last, ok, err := a.store.LastSuccess(ctx)
	if err != nil {
		return notify.StateStatus{}, err
	}
	runs, err := a.store.RunsSince(ctx, since)
	if err != nil {
		return notify.StateStatus{}, err
	}
	st := notify.StateStatus{
		Configured: v.Ready(),
		Paused:     v.Ready() && !v.Enabled,
		Schedule:   v.Schedule,
		Since:      v.UpdatedAt,
	}
	if ok {
		st.LastSuccess = last.FinishedAt
	}
	for _, r := range runs {
		st.Runs = append(st.Runs, notify.StateRun{
			Trigger: string(r.Trigger), Status: r.Status, Error: r.Error,
			From: r.FromVersion, To: r.ToVersion, Size: r.SizeBytes, At: r.FinishedAt,
		})
	}
	return st, nil
}

// PruneStateRuns drops journal rows finished before before.
func (a stateDigestAdapter) PruneStateRuns(ctx context.Context, before time.Time) error {
	return a.store.PruneRuns(ctx, before)
}
