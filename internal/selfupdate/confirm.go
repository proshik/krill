package selfupdate

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConfirmStartup settles whatever the previous process's update left in
// runDir. Call it once, as soon as this process serves requests: finding a
// pending marker for our own version is what cancels the dead-man timer.
//
// It is gated only on running under systemd, not on the directory probes of
// Supported: confirming never writes to the unit directory, its removals are
// best effort, and skipping it would let the timer revert a good update. It
// first waits up to confirmWait for the job slot and, once it has it, holds
// it to the end, so a Start or Rollback arriving meanwhile gets ErrJobRunning
// instead of racing it over the same markers. When the wait expires (or ctx
// ends) it goes ahead without the slot: letting the revert timer undo a good
// update would be worse.
func (u *Updater) ConfirmStartup(ctx context.Context) {
	if ok, _ := u.underSystemd(); !ok {
		return
	}
	if u.waitForSlot(ctx) {
		defer u.release()
	} else {
		slog.Error("self-update: could not take the job slot, confirming the startup without it",
			"wait", u.confirmWait, "ctx_err", ctx.Err())
	}
	if u.binPath != "" {
		removeStaged(filepath.Dir(u.binPath))
	}

	// The first result found is the one reported. It is recorded right away,
	// before any timer is stopped (which can retry for seconds), so a page
	// load just after the restart already shows it.
	reported := false
	report := func(r Result) {
		if reported {
			return
		}
		reported = true
		u.mu.Lock()
		u.last = &r
		u.mu.Unlock()
	}

	pendingPath := u.markerPath(pendingMarkerName)
	var pending pendingMarker
	switch err := readJSONMarker(pendingPath, &pending); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		// Safer to cancel a revert we cannot reason about than to leave it armed.
		slog.Error("self-update: unreadable pending marker, cancelling the revert timer", "err", err)
		removeFile(pendingPath)
		u.stopTimerWithRetry(ctx)
	case pending.To == u.current:
		// The marker goes first: a timer we then fail to stop finds no marker
		// and exits without reverting.
		removeFile(pendingPath)
		report(Result{From: pending.From, To: pending.To, OK: true})
		u.stopTimerWithRetry(ctx)
		slog.Info("self-update confirmed", "from", pending.From, "to", pending.To)
	default:
		// The previous process died after writing the marker but before it
		// swapped the binary: we are still the old version.
		removeFile(pendingPath)
		report(Result{From: pending.From, To: pending.To, Reason: "interrupted"})
		u.stopTimerWithRetry(ctx)
		u.removePrevIfRunning()
		slog.Warn("self-update was interrupted before the new binary was installed",
			"from", pending.From, "to", pending.To, "running", u.current)
	}

	revertedPath := u.markerPath(revertedMarkerName)
	switch data, err := readMarker(revertedPath); {
	case errors.Is(err, fs.ErrNotExist):
	default:
		if err != nil {
			slog.Error("self-update: unreadable reverted marker", "err", err)
		}
		// Written by the revert script; only a well-formed tag is echoed back.
		tag := strings.TrimSpace(string(data))
		if !ValidTag(tag) {
			tag = ""
		}
		removeFile(revertedPath)
		report(Result{From: u.current, To: tag, Reason: "reverted"})
		slog.Warn("self-update was rolled back by the dead-man timer", "to", tag, "running", u.current)
	}

	rolledbackPath := u.markerPath(rolledbackMarkerName)
	var rolled rolledbackMarker
	switch err := readJSONMarker(rolledbackPath, &rolled); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		slog.Error("self-update: unreadable rollback marker", "err", err)
		removeFile(rolledbackPath)
	default:
		removeFile(rolledbackPath)
		report(Result{From: rolled.From, To: rolled.To, OK: true, Reason: "rollback"})
		slog.Info("self-update rollback completed", "from", rolled.From, "to", rolled.To)
	}
}

// waitForSlot takes the job slot, retrying every confirmPoll for up to
// confirmWait, and reports whether it got it. Waiting is always short in
// practice: a launched job cannot coexist with a pending marker, because
// Start and Rollback refuse with ErrPending while one exists, so whoever
// holds the slot then is only a request still validating, about to be
// refused and to release it. Skipping confirmation instead would leave the
// marker and the revert timer in place to undo a good update.
func (u *Updater) waitForSlot(ctx context.Context) bool {
	deadline := time.Now().Add(u.confirmWait)
	poll := max(u.confirmPoll, time.Millisecond)
	for {
		if u.claim() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
		}
	}
}

// removePrevIfRunning removes <bin>.prev when it is the running binary
// itself — the previous process died between linking it and renaming the new
// binary into place — so Previous does not offer a "rollback" to the version
// already running.
func (u *Updater) removePrevIfRunning() {
	if u.binPath == "" {
		return
	}
	prev := u.binPath + prevSuffix
	binInfo, err := os.Stat(u.binPath)
	if err != nil {
		return
	}
	prevInfo, err := os.Stat(prev)
	if err != nil || !os.SameFile(binInfo, prevInfo) {
		return
	}
	removeFile(prev)
	slog.Info("self-update: removed a previous binary that was the running one", "path", prev)
}

// stopTimerWithRetry cancels the revert timer, trying three times with a
// growing pause, and logs if it never succeeds.
func (u *Updater) stopTimerWithRetry(ctx context.Context) {
	const attempts = 3
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = u.runCmd(ctx, u.stopTimerCmd()); err == nil {
			return
		}
		if attempt < attempts && u.stopBackoff > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(u.stopBackoff * time.Duration(attempt)):
			}
		}
	}
	slog.Error("self-update: cancelling the revert timer failed", "unit", u.revertUnit, "err", err)
}
