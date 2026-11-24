package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/proshik/krill/internal/oplock"
)

// A deploy's phase, as far as stopping its application is concerned.
type deployPhase int

const (
	// phaseQueued: the job waits in the queue.
	phaseQueued deployPhase = iota
	// phasePreparing: the worker runs it but has not touched the service yet
	// (resolving, building, predeploy, placement labels).
	phasePreparing
	// phaseApplying: the service spec is being written or has been written.
	// From here a stop no longer cancels the deploy; it scales what it wrote.
	phaseApplying
)

// tracked is one accepted deploy until its outcome is recorded.
type tracked struct {
	appID  int64
	phase  deployPhase
	cancel context.CancelCauseFunc // the job's context, once the worker runs it

	// stoppedBy is set when the application is stopped while this deploy is
	// open; it names who stopped it.
	stoppedBy string
	// recorded is set when StopApp recorded the deployment as cancelled. No one
	// else records it then: the worker drops it, or abandons it on its way out.
	recorded bool
}

// errStoppedBeforeApply ends a deploy whose application was stopped before the
// deploy applied its service spec.
var errStoppedBeforeApply = errors.New("the application was stopped before this deploy was applied")

// stopActorUnknown names a stop whose caller is not known.
const stopActorUnknown = "a user"

func stoppedBeforeApplyMessage(by string) string {
	if by == "" {
		by = stopActorUnknown
	}
	return "stopped by " + by + " before it was applied"
}

// track registers an accepted deploy (before it reaches the queue).
func (d *Deployer) track(deployID, appID int64) {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	if d.tracked == nil {
		d.tracked = map[int64]*tracked{}
	}
	d.tracked[deployID] = &tracked{appID: appID, phase: phaseQueued}
}

func (d *Deployer) untrack(deployID int64) {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	delete(d.tracked, deployID)
}

// begin moves a deploy from the queue to the worker. It reports false for one
// its application's stop already recorded as cancelled: the worker drops it.
// A deploy nobody tracked (run driven directly, as tests do) begins as is.
func (d *Deployer) begin(deployID int64, cancel context.CancelCauseFunc) bool {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	if t == nil {
		return true
	}
	if t.recorded {
		delete(d.tracked, deployID)
		return false
	}
	t.phase = phasePreparing
	t.cancel = cancel
	return true
}

// enterApply is the point of no return, called just before the service spec is
// written while the app's routing lock is held — the lock StopApp waits for
// before it scales the service. Either the stop came first and the deploy is
// refused here, or the deploy passes and the stop scales the spec it writes:
// never both, never neither.
func (d *Deployer) enterApply(deployID int64) error {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	if t == nil {
		return nil
	}
	if t.recorded || t.stoppedBy != "" {
		return errStoppedBeforeApply
	}
	t.phase = phaseApplying
	return nil
}

// stopRecorded reports whether StopApp recorded this deploy as cancelled.
func (d *Deployer) stopRecorded(deployID int64) bool {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	return t != nil && t.recorded
}

// stoppedBy names who stopped the app while this deploy was open; "" if no one.
func (d *Deployer) stoppedBy(deployID int64) string {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	if t := d.tracked[deployID]; t != nil {
		return t.stoppedBy
	}
	return ""
}

// abandon forgets a deploy the worker leaves without recording, because its
// stop recorded it already.
func (d *Deployer) abandon(deployID int64) {
	if d.stopRecorded(deployID) {
		d.untrack(deployID)
	}
}

// settleRecord is consulted by finishAs before it records an outcome. It
// returns false when StopApp recorded the deploy already (nothing more may be
// written), and otherwise whether the application was stopped meanwhile — then
// the stop owns the app's status and no deploy alert goes out. Either way the
// deploy is forgotten: its outcome is about to be final.
func (d *Deployer) settleRecord(deployID int64) (record, stopped bool) {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	if t == nil {
		return true, false
	}
	delete(d.tracked, deployID)
	if t.recorded {
		return false, true
	}
	return true, t.stoppedBy != ""
}

// stopWaitLimit caps how long StopApp waits for a deploy applying its spec to
// let go of the app's routing lock.
var stopWaitLimit = 30 * time.Second

// StopApp stops an application and every deploy of it that has not applied its
// service spec yet. A queued deploy is recorded as stopped and never runs; one
// the worker is preparing (resolving, building, running its predeploy command)
// has its context canceled and is recorded as stopped at once, so the
// application's in-flight slot and its organization's are free when StopApp
// returns. stop (scaling the service to zero) runs afterwards under the app's
// routing lock, which a deploy holds from its last check to the end of its
// ServiceDeploy: a deploy that got there first has its new spec scaled down; a
// deploy that did not is refused at that check. A deploy that applied its spec
// already keeps running until it sees the service stopped, then records that.
// by names who stopped the app in the deploy log and the deployment's error.
// It returns the deployments it cancelled.
func (d *Deployer) StopApp(ctx context.Context, appID int64, by string, stop func(context.Context) error) ([]int64, error) {
	msg := stoppedBeforeApplyMessage(by)
	if by == "" {
		by = stopActorUnknown
	}
	var cancelled []int64
	d.trackMu.Lock()
	for id, t := range d.tracked {
		if t.appID != appID || t.recorded {
			continue
		}
		if t.stoppedBy == "" {
			t.stoppedBy = by
		}
		if t.phase == phaseApplying {
			continue
		}
		t.recorded = true
		if t.cancel != nil {
			t.cancel(errStoppedBeforeApply)
		}
		cancelled = append(cancelled, id)
	}
	d.trackMu.Unlock()

	for _, id := range cancelled {
		fmt.Fprintf(d.hub.Writer(id), "⏹ %s\n", msg)
		full := d.hub.Close(id)
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := d.store.FinishDeployment(wctx, id, "error", "", msg, full); err != nil {
			slog.Error("record stopped deployment failed", "deploy", id, "app", appID, "err", err)
		}
		cancel()
		slog.Info("deploy stopped before it was applied", "deploy", id, "app", appID)
	}

	lctx, cancel := context.WithTimeout(ctx, stopWaitLimit)
	defer cancel()
	key := oplock.AppRouting(appID)
	if err := oplock.Acquire(lctx, key); err != nil {
		return cancelled, fmt.Errorf("wait for the app's deploy to finish applying: %w", err)
	}
	defer oplock.Release(key)
	return cancelled, stop(ctx)
}
