package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/statebackup"
)

// stateDumpCleanupTimeout bounds the startup removal of leftover dump
// containers; it runs in the background, so a slow Docker never holds boot.
const stateDumpCleanupTimeout = 30 * time.Second

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

// scheduledStateRun is the state backup scheduler's run function. It skips
// a run while a self-update job is active (that job's own pre-update snapshot
// covers the moment) and one that finds the slot taken; neither is alerted
// or journalled.
func scheduledStateRun(svc *statebackup.Service, updating func() bool) backup.RunFunc {
	return func(context.Context, int64) {
		if updating() {
			slog.Info("scheduled state backup skipped: an update is in progress")
			return
		}
		switch err := svc.Start(statebackup.TriggerSchedule); {
		case err == nil:
		case errors.Is(err, statebackup.ErrStateBackupRunning):
			slog.Info("scheduled state backup skipped: another state backup is still running")
		case errors.Is(err, statebackup.ErrNotConfigured):
			slog.Info("scheduled state backup skipped: Krill backups are not set up or are paused")
		default:
			slog.Error("scheduled state backup could not start", "err", err)
		}
	}
}

// cleanupStateDumps removes dump containers a crashed run left behind: their
// environment still holds the database password.
func cleanupStateDumps(ctx context.Context, runner docker.OneOffRunner) {
	ctx, cancel := context.WithTimeout(ctx, stateDumpCleanupTimeout)
	defer cancel()
	n, err := runner.RemoveContainersByLabel(ctx, docker.OneOffRoleLabel, statebackup.StateDumpLabelValue)
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
