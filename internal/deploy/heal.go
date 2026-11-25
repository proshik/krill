package deploy

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
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

// StaleApp is an application whose stored status says it runs, or that its
// last deploy failed, while none of its deployments is running: a candidate
// for "stopped" when its service turns out to be scaled to zero.
type StaleApp struct {
	ID           int64
	Status       string    // the stored status as listed
	Seen         time.Time // the row's updated_at when listed
	LastFinished time.Time // when its latest deployment ended
}

// StatusHealer is the store side of HealStuckStatuses.
type StatusHealer interface {
	ListStuckDeployingApps(ctx context.Context) ([]StuckApp, error)
	// HealAppStatus writes status only while the app is still stuck and its
	// row unchanged since it was listed (seen), and reports whether it did.
	HealAppStatus(ctx context.Context, appID int64, status string, seen time.Time) (bool, error)
	ListStaleStoppedCandidates(ctx context.Context) ([]StaleApp, error)
	// HealStoppedAppStatus sets the app idle only while its row still holds
	// from and is unchanged since it was listed (seen), and no deployment of
	// it is running; it reports whether it did.
	HealStoppedAppStatus(ctx context.Context, appID int64, from string, seen time.Time) (bool, error)
}

// HealEngine is the engine side of HealStuckStatuses.
type HealEngine interface {
	ServiceProgress(ctx context.Context, name string, exclude []string) (docker.ServiceProgress, error)
	ServiceStates(ctx context.Context, names []string) (map[string]docker.ServiceState, error)
}

// healLookupTimeout bounds each Docker and database call of a pass: one that
// never answers must not hold back the apps after it, or every later pass.
const healLookupTimeout = 5 * time.Second

// stoppedHealAge is how long a service must have been scaled to zero, without
// any change, before a stored "running" or "error" is taken for a stop. A
// volume restore scales the app to zero for its duration (at most 30 minutes)
// and back up; an app inside that window is not stopped.
const stoppedHealAge = time.Hour

// stoppedPassBudget bounds the stopped-app half of a pass, so a slow daemon
// cannot hold back the next pass's apps left deploying; what it did not reach
// waits for the next pass.
const stoppedPassBudget = 20 * time.Second

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
// Swarm reports for its service — the apps left "deploying" with no deploy
// running, then the apps stored "running" or "error" whose service has stood
// at zero replicas for stoppedHealAge (healStopped) — and returns how many it
// changed. Nothing is notified: the deploy's own outcome was already recorded
// (or failed at startup), the health watcher reports a service that is down,
// and every reader of an app whose service is at zero already derives "idle"
// from it (the stored value shows only while Swarm does not answer or the
// service is gone).
func HealStuckStatuses(ctx context.Context, s StatusHealer, eng HealEngine, now time.Time) (int, error) {
	healed, err := healDeploying(ctx, s, eng, now)
	if ctx.Err() != nil {
		return healed, ctx.Err()
	}
	n, serr := healStopped(ctx, s, eng, now)
	if err == nil {
		err = serr
	}
	return healed + n, err
}

// healDeploying sets the status of the apps left "deploying".
func healDeploying(ctx context.Context, s StatusHealer, eng HealEngine, now time.Time) (int, error) {
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

// stoppedLongEnough reports whether a service is a stopped replicated one:
// scaled to zero, no replica left, its last update settled and unchanged for
// stoppedHealAge. A stored "error" also needs the service to have changed after
// the app's latest deployment ended: a start that failed before it touched a
// service stopped long ago recorded an error the stop does not explain. A
// stored "running" does not — a deploy that wrote it left replicas behind, so
// a service at zero was stopped since, whatever the two clocks say. A global
// service at zero is waiting for an eligible node.
func stoppedLongEnough(p docker.ServiceProgress, last StaleApp, now time.Time) bool {
	if last.Status == StatusError && !p.UpdatedAt.After(last.LastFinished) {
		return false
	}
	return p.Found && !p.Global && p.Desired == 0 && p.Running == 0 &&
		docker.UpdateSettled(p.UpdateState) &&
		!p.UpdatedAt.IsZero() && now.Sub(p.UpdatedAt) >= stoppedHealAge
}

// healStopped sets idle — what Stop writes — the apps stored "running" or
// "error" whose service has been scaled to zero for stoppedHealAge with no
// deploy of theirs running. A Stop through the agent API before v0.13.2
// scaled the service without writing the status (an app stopped through
// MCP on v0.9.4 read "running" at 0/0 for good). One bulk lookup picks the replicated services at
// zero; only those are read in detail. An app a volume restore holds at zero
// (oplock.AppRestore) is skipped, the lock held from the read to the write.
func healStopped(ctx context.Context, s StatusHealer, eng HealEngine, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, stoppedPassBudget)
	defer cancel()
	lctx, lcancel := context.WithTimeout(ctx, healLookupTimeout)
	apps, err := s.ListStaleStoppedCandidates(lctx)
	lcancel()
	if err != nil || len(apps) == 0 {
		return 0, err
	}
	// A pass the budget cuts short must not keep missing the same apps.
	rand.Shuffle(len(apps), func(i, j int) { apps[i], apps[j] = apps[j], apps[i] })
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = docker.ServiceName(a.ID)
	}
	sctx, scancel := context.WithTimeout(ctx, healLookupTimeout)
	states, err := eng.ServiceStates(sctx, names)
	scancel()
	if err != nil {
		return 0, err
	}
	healed := 0
	var firstErr error
	for _, a := range apps {
		if ctx.Err() != nil {
			return healed, ctx.Err()
		}
		if st := states[docker.ServiceName(a.ID)]; !st.Found || st.Global || st.Desired != 0 {
			continue
		}
		ok, err := healStoppedApp(ctx, s, eng, a, now)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			healed++
			slog.Info("application status contradicted its service scaled to zero with no deploy running; set idle", "app", a.ID, "was", a.Status)
		}
	}
	return healed, firstErr
}

// healStoppedApp reads one app's service and sets the app idle when it is
// stopped long enough, unless a volume restore of the app is under way. The
// app's restore lock is held from the read to the write, so no restore can
// start in between.
func healStoppedApp(ctx context.Context, s StatusHealer, eng HealEngine, a StaleApp, now time.Time) (bool, error) {
	name := oplock.AppRestore(a.ID)
	if !oplock.TryAcquire(name) {
		return false, nil
	}
	defer oplock.Release(name)
	if oplock.Marked(name) {
		return false, nil
	}
	pctx, pcancel := context.WithTimeout(ctx, healLookupTimeout)
	p, err := eng.ServiceProgress(pctx, docker.ServiceName(a.ID), nil)
	pcancel()
	if err != nil {
		slog.Warn("heal: could not read the service of an app scaled to zero", "app", a.ID, "err", err)
		return false, err
	}
	if !stoppedLongEnough(p, a, now) {
		return false, nil
	}
	wctx, wcancel := context.WithTimeout(ctx, healLookupTimeout)
	ok, err := s.HealStoppedAppStatus(wctx, a.ID, a.Status, a.Seen)
	wcancel()
	if err != nil {
		slog.Warn("heal: could not write the status of an app scaled to zero", "app", a.ID, "err", err)
		return false, err
	}
	return ok, nil
}

// StartStatusHeal runs HealStuckStatuses now and then every interval until ctx
// ends or the returned stop is called. Call it after FailOrphanedDeployments,
// so the deploys a crash left running count as ended.
func StartStatusHeal(ctx context.Context, s StatusHealer, eng HealEngine, interval time.Duration) (stop func()) {
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
