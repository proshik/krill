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

// healPassTimeout bounds one pass: a Docker call that never answers must not
// hold back every later pass.
const healPassTimeout = 30 * time.Second

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
		// first deploy), while a stopped global app has its service removed.
		if last.LastStatus == "error" {
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
	stuck, err := s.ListStuckDeployingApps(ctx)
	if err != nil {
		return 0, err
	}
	healed := 0
	for _, a := range stuck {
		p, err := eng.ServiceProgress(ctx, docker.ServiceName(a.ID), nil)
		if err != nil {
			return healed, err
		}
		status := healedStatus(p, a, now)
		if status == "" {
			continue
		}
		ok, err := s.HealAppStatus(ctx, a.ID, status, a.Seen)
		if err != nil {
			return healed, err
		}
		if ok {
			healed++
			slog.Info("application status was left deploying with no deploy running; set from the service", "app", a.ID, "status", status)
		}
	}
	return healed, nil
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
			pctx, pcancel := context.WithTimeout(ctx, healPassTimeout)
			if _, err := HealStuckStatuses(pctx, s, eng, time.Now()); err != nil && ctx.Err() == nil {
				slog.Warn("could not heal stuck application statuses", "err", err)
			}
			pcancel()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return cancel
}
