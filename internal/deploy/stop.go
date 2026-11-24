package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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

	// stoppedBy is set once a stop of the app scaled the service this deploy
	// applied (phaseApplying only); it names who stopped it.
	stoppedBy string

	// recorded is set when a stop cancelled this deploy before it applied its
	// spec: from then on only StopApp records its outcome (stopMsg, with the
	// log it had so far), and the worker drops or abandons it.
	recorded  bool
	persisted bool   // the cancellation is in the database
	stopMsg   string // the deployment's error
	stopLog   string // the deploy log up to the stop
	// workerDone: no worker step will look at this deploy again (it was
	// dropped from the queue, or its run returned). A cancelled deploy is
	// forgotten once it is both persisted and workerDone, so a failed write
	// can be retried by the next stop.
	workerDone bool

	// finishing is set when finishAs has taken the outcome; done closes when
	// the outcome and the app's status are written. A stop waits for it, so
	// a deploy finishing at that moment cannot overwrite the stop's status.
	finishing bool
	done      chan struct{}

	// persisting is set while one stop writes the cancellation, so a second
	// stop running at the same time neither takes the log a second time
	// (getting an empty one) nor overwrites the row with it.
	persisting bool
	// exited closes when a cancelled run has returned (its build or predeploy
	// container is gone); nil for a deploy the worker never started.
	exited     chan struct{}
	exitedOnce sync.Once
}

// errStoppedBeforeApply ends a deploy whose application was stopped before the
// deploy applied its service spec.
var errStoppedBeforeApply = errors.New("the application was stopped before this deploy was applied")

// errStoppedMeanwhile ends a system deploy (the network migration) of an app
// that was stopped after the migration chose to move it: redeploying it would
// start it again.
var errStoppedMeanwhile = errors.New("skipped: the application was stopped")

// stopActorUnknown names a stop whose caller is not known.
const stopActorUnknown = "a user"

func stoppedBeforeApplyMessage(by string) string {
	return "stopped by " + by + " before it was applied"
}

// track registers an accepted deploy (before it reaches the queue).
func (d *Deployer) track(deployID, appID int64) {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	if d.tracked == nil {
		d.tracked = map[int64]*tracked{}
	}
	d.tracked[deployID] = &tracked{appID: appID, phase: phaseQueued, done: make(chan struct{})}
}

// forgetLocked drops a cancelled deploy once nothing needs it any more.
// trackMu must be held.
func (d *Deployer) forgetLocked(deployID int64, t *tracked) {
	if t.persisted && t.workerDone {
		delete(d.tracked, deployID)
	}
}

// claimFinish gives a path that records a deploy's outcome by itself (the
// shutdown drain, a send refused at shutdown) the right to do so. It is
// false when a stop cancelled the deploy: the stop records it. Otherwise the
// deploy is forgotten here, atomically, so a stop arriving later cannot
// record it too.
func (d *Deployer) claimFinish(deployID int64) bool {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	if t == nil {
		return true
	}
	if t.recorded {
		t.workerDone = true
		d.forgetLocked(deployID, t)
		return false
	}
	delete(d.tracked, deployID)
	return true
}

// begin moves a deploy from the queue to the worker. It reports false for one
// a stop already cancelled: the worker drops it. A deploy nobody tracked (run
// driven directly, as tests do) begins as is.
func (d *Deployer) begin(deployID int64, cancel context.CancelCauseFunc) bool {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	if t == nil {
		return true
	}
	if t.recorded {
		t.workerDone = true
		d.forgetLocked(deployID, t)
		return false
	}
	t.phase = phasePreparing
	t.cancel = cancel
	t.exited = make(chan struct{})
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
	if t.recorded {
		return errStoppedBeforeApply
	}
	t.phase = phaseApplying
	return nil
}

// stopRecorded reports whether a stop cancelled this deploy.
func (d *Deployer) stopRecorded(deployID int64) bool {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	return t != nil && t.recorded
}

// stoppedBy names who stopped the app after this deploy applied its spec; ""
// if no one has.
func (d *Deployer) stoppedBy(deployID int64) string {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	if t := d.tracked[deployID]; t != nil {
		return t.stoppedBy
	}
	return ""
}

// abandon is the worker leaving a deploy without recording it, because its
// stop records it.
func (d *Deployer) abandon(deployID int64) {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	if t := d.tracked[deployID]; t != nil && t.recorded {
		t.workerDone = true
		if t.exited != nil {
			t.exitedOnce.Do(func() { close(t.exited) })
		}
		d.forgetLocked(deployID, t)
	}
}

// settleRecord is consulted by finishAs before it records an outcome. It
// returns false when a stop cancelled the deploy (the stop records it), and
// otherwise whether the application was stopped after the deploy applied
// its spec — then the stop owns the app's status and no deploy alert goes
// out. The deploy is marked finishing; finished closes it.
func (d *Deployer) settleRecord(deployID int64) (record, stopped bool) {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	if t == nil {
		return true, false
	}
	if t.recorded {
		return false, true
	}
	t.finishing = true
	return true, t.stoppedBy != ""
}

// finished forgets a deploy whose outcome finishAs has written and wakes a
// stop waiting for it.
func (d *Deployer) finished(deployID int64) {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	if t := d.tracked[deployID]; t != nil && t.finishing {
		delete(d.tracked, deployID)
		close(t.done)
	}
}

// compactQueue takes the jobs a stop cancelled out of the queue, keeping the
// order of the others. A cancelled job used to keep its slot until the worker
// reached it, and its in-flight caps were already free, so deploying and
// stopping in a loop while a long build ran could fill the shared queue and
// refuse every other tenant's deploys.
func (d *Deployer) compactQueue() {
	d.queue.remove(func(j job) bool { return d.dropCancelled(j.deployID) })
}

// dropCancelled reports whether a queued job was cancelled by a stop, and
// releases it if so.
func (d *Deployer) dropCancelled(deployID int64) bool {
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t := d.tracked[deployID]
	if t == nil || !t.recorded {
		return false
	}
	t.workerDone = true
	d.forgetLocked(deployID, t)
	return true
}

// stopWaitLimit caps how long StopApp waits for what it has to let finish
// first: an enqueue in progress, a cancelled run winding down, a deploy
// applying its spec or writing its outcome.
var stopWaitLimit = 30 * time.Second

// StopApp stops an application and every deploy of it that has not applied its
// service spec yet.
//
// A queued deploy is taken out of the queue and recorded as stopped; one the
// worker is preparing (resolving, building, running its predeploy command) has
// its context canceled and is recorded as stopped at once, so the
// application's in-flight slot and its organization's are free when StopApp
// returns. This happens under admitMu, so no deploy of the app can be between
// its database row and its tracking. A cancelled run is waited for until it
// has wound down (its build killed, its predeploy container removed), and a
// deploy recording its outcome right now is waited for, so its status cannot
// land after the stop's.
//
// stop (scaling the service to zero) then runs under the app's routing lock,
// which a deploy holds from its last check (enterApply) to the end of its
// ServiceDeploy: a deploy that got there first has its new spec scaled down; a
// deploy that did not is refused at that check. Only once stop succeeded is a
// deploy that already applied its spec marked stopped, so it ends quietly when
// it sees the service at zero; a failed stop leaves it to report normally. One
// that started recording its outcome meanwhile is waited for instead.
//
// by names who stopped the app in the deploy log and the deployment's error.
// It returns the deployments it cancelled. An error writing a cancellation is
// returned after stop ran; the next stop of the app retries the write.
func (d *Deployer) StopApp(ctx context.Context, appID int64, by string, stop func(context.Context) error) ([]int64, error) {
	if by == "" {
		by = stopActorUnknown
	}
	lctx, cancel := context.WithTimeout(ctx, stopWaitLimit)
	defer cancel()
	if err := d.lockAdmit(lctx); err != nil {
		return nil, fmt.Errorf("wait for a deploy being queued: %w", err)
	}
	var cancelled, applying []int64
	var waits []chan struct{}
	d.trackMu.Lock()
	for id, t := range d.tracked {
		if t.appID != appID {
			continue
		}
		switch {
		case t.recorded:
			if !t.persisted {
				cancelled = append(cancelled, id) // an earlier write failed
			}
		case t.finishing:
			waits = append(waits, t.done)
		case t.phase == phaseApplying:
			applying = append(applying, id)
		default:
			t.recorded = true
			t.stopMsg = stoppedBeforeApplyMessage(by)
			if t.cancel != nil {
				t.cancel(errStoppedBeforeApply)
			}
			cancelled = append(cancelled, id)
		}
		if t.recorded && t.exited != nil {
			waits = append(waits, t.exited)
		}
	}
	d.trackMu.Unlock()
	d.compactQueue()
	d.admitMu.Unlock()

	var persistErr error
	for _, id := range cancelled {
		if err := d.persistStop(ctx, id); err != nil {
			slog.Error("record stopped deployment failed", "deploy", id, "app", appID, "err", err)
			persistErr = errors.Join(persistErr, err)
			continue
		}
		slog.Info("deploy stopped before it was applied", "deploy", id, "app", appID)
	}

	if err := waitAll(lctx, waits); err != nil {
		return cancelled, fmt.Errorf("wait for the app's deploy to wind down: %w", err)
	}
	key := oplock.AppRouting(appID)
	if err := oplock.Acquire(lctx, key); err != nil {
		return cancelled, fmt.Errorf("wait for the app's deploy to finish applying: %w", err)
	}
	err := stop(ctx)
	oplock.Release(key)
	if err != nil {
		return cancelled, err
	}
	waits = waits[:0]
	d.trackMu.Lock()
	for _, id := range applying {
		t := d.tracked[id]
		switch {
		case t == nil:
		case t.finishing:
			waits = append(waits, t.done) // decided before it could see the stop
		default:
			t.stoppedBy = by
		}
	}
	d.trackMu.Unlock()
	if err := waitAll(lctx, waits); err != nil {
		return cancelled, fmt.Errorf("wait for the app's deploy to record its outcome: %w", err)
	}
	if persistErr != nil {
		return cancelled, fmt.Errorf("record a stopped deployment: %w", persistErr)
	}
	return cancelled, nil
}

// lockAdmit takes admitMu, giving up when ctx ends: an enqueue holds it
// across database calls, and a stop must not hang behind a stuck one.
func (d *Deployer) lockAdmit(ctx context.Context) error {
	for !d.admitMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

func waitAll(ctx context.Context, chans []chan struct{}) error {
	for _, ch := range chans {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// persistStop writes a cancelled deploy's outcome. One call at a time takes
// it: the first takes the deploy log (closing its feed), and a failed write
// leaves it for the next stop, which writes the same log.
func (d *Deployer) persistStop(ctx context.Context, deployID int64) error {
	d.trackMu.Lock()
	t := d.tracked[deployID]
	if t == nil || t.persisted || t.persisting {
		d.trackMu.Unlock()
		return nil
	}
	t.persisting = true
	msg := t.stopMsg
	d.trackMu.Unlock()

	if d.hub.Active(deployID) {
		fmt.Fprintf(d.hub.Writer(deployID), "⏹ %s\n", msg)
		full := d.hub.Close(deployID)
		d.trackMu.Lock()
		t.stopLog = full
		d.trackMu.Unlock()
	}
	d.trackMu.Lock()
	full := t.stopLog
	d.trackMu.Unlock()

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := d.store.FinishDeployment(wctx, deployID, "error", "", msg, full)
	d.trackMu.Lock()
	defer d.trackMu.Unlock()
	t.persisting = false
	if err != nil {
		return err
	}
	t.persisted = true
	d.forgetLocked(deployID, t)
	return nil
}
