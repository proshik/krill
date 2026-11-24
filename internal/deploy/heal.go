package deploy

import (
	"context"
	"log/slog"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// StuckApp is an application whose stored status is "deploying" while none of
// its deployments is running: the deploy that set it was interrupted (Krill
// stopped or crashed before it recorded an outcome), or it ended on a release
// that left the status behind. LastFinished is when its latest deployment
// ended.
type StuckApp struct {
	ID           int64
	LastFinished time.Time
}

// StatusHealer is the store side of HealStuckStatuses.
type StatusHealer interface {
	ListStuckDeployingApps(ctx context.Context) ([]StuckApp, error)
	// HealAppStatus writes status only while the app is still stuck, and
	// reports whether it did.
	HealAppStatus(ctx context.Context, appID int64, status string) (bool, error)
}

// ServiceStateReader is the engine side of HealStuckStatuses.
type ServiceStateReader interface {
	ServiceStates(ctx context.Context, names []string) (map[string]docker.ServiceState, error)
}

// healedStatus is the status a stuck app gets from its live service, or ""
// to leave it for a later pass. A service that has not caught up yet is given
// as long as a watched deploy would have (settleTimeout after the last
// deployment ended) before it counts as failed.
func healedStatus(st docker.ServiceState, lastFinished, now time.Time) string {
	switch {
	case !st.Found || st.Desired == 0:
		// No service (never created, or a stopped global app) or scaled to zero.
		return StatusIdle
	case st.Running >= st.Desired:
		return StatusRunning
	case now.Sub(lastFinished) >= settleTimeout:
		return StatusError
	}
	return ""
}

// HealStuckStatuses rewrites the stored status of every stuck app from what
// Swarm reports for its service, and returns how many it changed. Nothing is
// notified: the deploy's own outcome was already recorded (or failed at
// startup), and the health watcher reports a service that is down.
func HealStuckStatuses(ctx context.Context, s StatusHealer, eng ServiceStateReader, now time.Time) (int, error) {
	stuck, err := s.ListStuckDeployingApps(ctx)
	if err != nil || len(stuck) == 0 {
		return 0, err
	}
	names := make([]string, len(stuck))
	for i, a := range stuck {
		names[i] = docker.ServiceName(a.ID)
	}
	states, err := eng.ServiceStates(ctx, names)
	if err != nil {
		return 0, err
	}
	healed := 0
	for _, a := range stuck {
		status := healedStatus(states[docker.ServiceName(a.ID)], a.LastFinished, now)
		if status == "" {
			continue
		}
		ok, err := s.HealAppStatus(ctx, a.ID, status)
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
func StartStatusHeal(ctx context.Context, s StatusHealer, eng ServiceStateReader, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if _, err := HealStuckStatuses(ctx, s, eng, time.Now()); err != nil && ctx.Err() == nil {
				slog.Warn("could not heal stuck application statuses", "err", err)
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
