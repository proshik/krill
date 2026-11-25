package deploy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// StuckApp is an application whose stored status is "deploying" while none of
// its deployments is running: the deploy that set it was interrupted (Krill
// stopped or crashed before it recorded an outcome), or it ended on a release
// that left the status behind.
type StuckApp struct {
	ID           int64
	Seen         time.Time // the row's updated_at when listed
	LastFinished time.Time // when its latest deployment ended
	LastStatus   string    // that deployment's status; "" when it has none
	LastError    string    // and its error message
}

// StatusHealer is the store side of HealStuckStatuses.
type StatusHealer interface {
	ListStuckDeployingApps(ctx context.Context) ([]StuckApp, error)
	// HealAppStatus writes status only while the app is still stuck and its
	// row unchanged since it was listed (seen), and reports whether it did.
	HealAppStatus(ctx context.Context, appID int64, status string, seen time.Time) (bool, error)
}

// ServiceProgressReader is the engine side of HealStuckStatuses.
type ServiceProgressReader interface {
	ServiceProgress(ctx context.Context, name string, exclude []string) (docker.ServiceProgress, error)
}

// healLookupTimeout bounds each Docker and database call of a pass: one that
// never answers must not hold back the apps after it, or every later pass.
const healLookupTimeout = 5 * time.Second

// stoppedOutcome reports whether a deployment's error records a stop of its
// app (the stop owns the app's status) rather than a failure. A shutdown's
// "interrupted: krill stopped …" is not one.
func stoppedOutcome(msg string) bool {
	return strings.HasPrefix(msg, "stopped by ") ||
		msg == "stopped before the service converged" ||
		msg == errStoppedMeanwhile.Error() ||
		msg == errStoppedBeforeApply.Error()
}

// healedStatus is the status a stuck app gets from its live service, or ""
// to leave it for a later pass. Whatever is still moving — a rolling update
// Swarm has not finished (the old replica can still be serving, so counting
// replicas would call it running), a global service waiting for an eligible
// node, a replica still starting — is given as long as a watched deploy would
// have (settleTimeout after the last deployment ended) before it counts as
// failed.
func healedStatus(p docker.ServiceProgress, last StuckApp, now time.Time) string {
	late := now.Sub(last.LastFinished) >= settleTimeout
	wait := func() string {
		if late {
			return StatusError
		}
		return ""
	}
	switch {
	case !p.Found:
		// No service: a deploy that never created it failed (an interrupted
		// first deploy), while a stopped global app has its service removed —
		// also when the stop cancelled the deploy and Krill exited before the
		// stop wrote the app's status.
		if last.LastStatus == "error" && !stoppedOutcome(last.LastError) {
			return StatusError
		}
		return StatusIdle
	case !docker.UpdateSettled(p.UpdateState):
		return wait()
	case strings.HasPrefix(p.UpdateState, "rollback"):
		// Swarm rolled the update back: the deploy failed, whatever runs now.
		return StatusError
	case p.Global && p.Desired == 0:
		return wait()
	case p.Desired == 0:
		return StatusIdle
	case p.Running >= p.Desired:
		return StatusRunning
	}
	return wait()
}

// HealStuckStatuses rewrites the stored status of every stuck app from what
// Swarm reports for its service, and returns how many it changed. Nothing is
// notified: the deploy's own outcome was already recorded (or failed at
// startup), and the health watcher reports a service that is down.
func HealStuckStatuses(ctx context.Context, s StatusHealer, eng ServiceProgressReader, now time.Time) (int, error) {
	lctx, lcancel := context.WithTimeout(ctx, healLookupTimeout)
	stuck, err := s.ListStuckDeployingApps(lctx)
	lcancel()
	if err != nil {
		return 0, err
	}
	healed := 0
	var firstErr error
	for _, a := range stuck {
		if ctx.Err() != nil {
			return healed, ctx.Err()
		}
		// One app's failed lookup (a service removed meanwhile, a slow
		// daemon) leaves that app for the next pass, not the ones after it.
		pctx, pcancel := context.WithTimeout(ctx, healLookupTimeout)
		p, err := eng.ServiceProgress(pctx, docker.ServiceName(a.ID), nil)
		pcancel()
		if err != nil {
			slog.Warn("heal: could not read the service of an app left deploying", "app", a.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		status := healedStatus(p, a, now)
		if status == "" {
			continue
		}
		wctx, wcancel := context.WithTimeout(ctx, healLookupTimeout)
		ok, err := s.HealAppStatus(wctx, a.ID, status, a.Seen)
		wcancel()
		if err != nil {
			slog.Warn("heal: could not write the status of an app left deploying", "app", a.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			healed++
			slog.Info("application status was left deploying with no deploy running; set from the service", "app", a.ID, "status", status)
		}
	}
	return healed, firstErr
}

// StartStatusHeal runs HealStuckStatuses now and then every interval until ctx
// ends or the returned stop is called. Call it after FailOrphanedDeployments,
// so the deploys a crash left running count as ended.
func StartStatusHeal(ctx context.Context, s StatusHealer, eng ServiceProgressReader, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if _, err := HealStuckStatuses(ctx, s, eng, time.Now()); err != nil && ctx.Err() == nil {
				slog.Warn("could not heal every stuck application status", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return cancel
}
