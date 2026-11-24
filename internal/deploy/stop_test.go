package deploy

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/docker"
)

// stopEngine records the order of service writes and can hold a deploy at
// the places a stop can meet it: resolving the image, the task snapshot taken
// under the routing lock just before the spec is applied, and ServiceDeploy.
type stopEngine struct {
	*mockEngine
	emu    sync.Mutex
	events []string

	resolveStarted chan struct{} // closed when ResolveDigest is entered
	resolveHold    bool          // ResolveDigest waits for its context to end

	snapshotStarted chan struct{} // closed when the pre-apply snapshot is entered
	snapshotRelease chan struct{} // the snapshot answers (ignoring ctx) once closed
	snapshotDone    bool

	deployStarted chan struct{} // closed when ServiceDeploy is entered
	deployRelease chan struct{} // ServiceDeploy returns once closed

	jitter bool // random short delays in every call (the race test)

	scaled map[string]bool // services scaled to zero
}

func newStopEngine() *stopEngine { return &stopEngine{mockEngine: &mockEngine{}} }

func (e *stopEngine) event(s string) {
	e.emu.Lock()
	defer e.emu.Unlock()
	e.events = append(e.events, s)
}

func (e *stopEngine) log() []string {
	e.emu.Lock()
	defer e.emu.Unlock()
	return append([]string(nil), e.events...)
}

func (e *stopEngine) pause() {
	if e.jitter {
		time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
	}
}

func (e *stopEngine) ResolveDigest(ctx context.Context, ref, _ string) (string, error) {
	e.pause()
	if e.resolveStarted != nil {
		close(e.resolveStarted)
		e.resolveStarted = nil
	}
	if e.resolveHold {
		e.resolveHold = false // only the first deploy is held
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "", nil
}

func (e *stopEngine) ServiceProgress(ctx context.Context, name string, baseline []string) (docker.ServiceProgress, error) {
	e.pause()
	e.emu.Lock()
	first := !e.snapshotDone
	e.snapshotDone = true
	e.emu.Unlock()
	if first && e.snapshotRelease != nil {
		close(e.snapshotStarted)
		<-e.snapshotRelease
		return docker.ServiceProgress{}, nil
	}
	e.emu.Lock()
	down := e.scaled[name]
	e.emu.Unlock()
	if down {
		return docker.ServiceProgress{Found: true, Desired: 0, TaskIDs: []string{"t-new"}}, nil
	}
	return e.mockEngine.ServiceProgress(ctx, name, baseline)
}

func (e *stopEngine) ServiceDeploy(ctx context.Context, s docker.ServiceSpec) error {
	e.pause()
	e.event("deploy-start")
	if e.deployStarted != nil {
		close(e.deployStarted)
	}
	if e.deployRelease != nil {
		<-e.deployRelease
	}
	err := e.mockEngine.ServiceDeploy(ctx, s)
	e.event("deploy-end")
	return err
}

func (e *stopEngine) ServiceScale(_ context.Context, name string, _ uint64) error {
	e.pause()
	e.event("scale")
	e.emu.Lock()
	defer e.emu.Unlock()
	if e.scaled == nil {
		e.scaled = map[string]bool{}
	}
	e.scaled[name] = true
	return nil
}

func (e *stopEngine) deploys() int {
	n := 0
	for _, ev := range e.log() {
		if ev == "deploy-start" {
			n++
		}
	}
	return n
}

func (e *stopEngine) scaler(appID int64) func(context.Context) error {
	return func(ctx context.Context) error { return e.ServiceScale(ctx, docker.ServiceName(appID), 0) }
}

// stopAsync runs StopApp off the test goroutine.
func stopAsync(d *Deployer, e *stopEngine, appID int64) <-chan []int64 {
	done := make(chan []int64, 1)
	go func() {
		ids, err := d.StopApp(context.Background(), appID, "alice@example.com", e.scaler(appID))
		if err != nil {
			ids = append(ids, -1)
		}
		done <- ids
	}()
	return done
}

func await(t *testing.T, done <-chan []int64) []int64 {
	t.Helper()
	select {
	case ids := <-done:
		for _, id := range ids {
			if id == -1 {
				t.Fatal("StopApp failed")
			}
		}
		return ids
	case <-time.After(5 * time.Second):
		t.Fatal("StopApp did not return")
	}
	return nil
}

func stopApp(t *testing.T, d *Deployer, e *stopEngine, appID int64) []int64 {
	t.Helper()
	ids, err := d.StopApp(t.Context(), appID, "alice@example.com", e.scaler(appID))
	if err != nil {
		t.Fatalf("StopApp: %v", err)
	}
	return ids
}

// A queued deploy is recorded stopped at once, frees the app's in-flight slot
// and never runs: before the fix the worker picked it up later and deployed
// the app Stop had just "stopped".
func TestStopCancelsQueuedDeploy(t *testing.T) {
	e := newStopEngine()
	st := newFakeStore(imageApp())
	d := newDeployer(e, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)

	id := d.Enqueue(1, TriggerManual)
	if id == 0 {
		t.Fatal("deploy not accepted")
	}
	if got := stopApp(t, d, e, 1); len(got) != 1 || got[0] != id {
		t.Fatalf("cancelled = %v, want [%d]", got, id)
	}
	if st.depStatus(id) != "error" || st.depError(id) != "stopped by alice@example.com before it was applied" {
		t.Fatalf("deployment = %q %q", st.depStatus(id), st.depError(id))
	}
	// The job left the queue, and the slot is free: a new deploy is
	// accepted right away and is the only job queued.
	if n := len(d.queue); n != 0 {
		t.Fatalf("%d jobs queued after the stop, want 0", n)
	}
	next, err := d.EnqueueErr(1, TriggerManual)
	if err != nil {
		t.Fatalf("deploy after stop refused: %v", err)
	}
	if j := <-d.queue; j.deployID != next || len(d.queue) != 0 {
		t.Fatalf("queued job %d, want only %d", j.deployID, next)
	}
	if st.depStatus(id) != "error" {
		t.Fatalf("stopped deployment rewritten to %q", st.depStatus(id))
	}
	if fn.was() {
		t.Fatal("a stopped deploy alerted as failed")
	}
}

// A deploy still resolving its image is interrupted and never applies its
// spec; the stop recorded it, so the worker adds nothing (no "deploy failed",
// no alert, no app status).
func TestStopCancelsPreparingImageDeploy(t *testing.T) {
	e := newStopEngine()
	e.resolveStarted, e.resolveHold = make(chan struct{}), true
	st := newFakeStore(imageApp())
	other := imageApp()
	other.ID = 2
	st.apps = map[int64]App{2: other}
	d := newDeployer(e, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)
	d.Start(context.Background())
	defer d.Stop()

	started := e.resolveStarted
	id := d.Enqueue(1, TriggerManual)
	<-started
	stopApp(t, d, e, 1)
	if st.depError(id) != "stopped by alice@example.com before it was applied" {
		t.Fatalf("deployment error = %q", st.depError(id))
	}
	st.SetStatus(t.Context(), 1, StatusIdle) // what the handler records
	// Let the worker run past the point where it would have applied.
	next := d.Enqueue(2, TriggerManual)
	waitFor(t, func() bool { return st.depStatus(next) != "" })
	for _, s := range e.deployedSpecs() {
		if s.Name == docker.ServiceName(1) {
			t.Fatal("the stopped deploy applied its spec")
		}
	}
	if st.appStatus(1) != StatusIdle {
		t.Fatalf("app status = %q, want idle", st.appStatus(1))
	}
	if fn.was() {
		t.Fatal("a stopped deploy alerted as failed")
	}
}

// A Dockerfile build in progress is cancelled through its context.
func TestStopCancelsRunningBuild(t *testing.T) {
	e := newStopEngine()
	bb := &blockingBuilder{started: make(chan struct{})}
	st := newFakeStore(dockerfileApp())
	other := imageApp()
	other.ID = 3
	st.apps = map[int64]App{3: other}
	d := newDeployer(e, bb, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(2, TriggerManual)
	<-bb.started
	await(t, stopAsync(d, e, 2))
	if !strings.Contains(st.depError(id), "before it was applied") {
		t.Fatalf("deployment error = %q", st.depError(id))
	}
	next := d.Enqueue(3, TriggerManual)
	waitFor(t, func() bool { return st.depStatus(next) != "" })
	for _, s := range e.deployedSpecs() {
		if s.Name == docker.ServiceName(2) {
			t.Fatal("the cancelled build was deployed")
		}
	}
}

func (e *stopEngine) deployedSpecs() []docker.ServiceSpec {
	e.mockEngine.mu.Lock()
	defer e.mockEngine.mu.Unlock()
	return append([]docker.ServiceSpec(nil), e.mockEngine.deployed...)
}

// Stop and the apply step race for the routing lock: a stop that marks the
// deploy while it is past its last await but before the spec is written
// wins, and the deploy is refused at its final check — even though the
// snapshot it was in ignored the canceled context.
func TestStopBeforeApplyCheckWins(t *testing.T) {
	e := newStopEngine()
	e.snapshotStarted, e.snapshotRelease = make(chan struct{}), make(chan struct{})
	st := newFakeStore(imageApp())
	d := newDeployer(e, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	var once sync.Once
	release := func() { once.Do(func() { close(e.snapshotRelease) }) }
	defer release() // before d.Stop, should the test fail early

	id := d.Enqueue(1, TriggerManual)
	<-e.snapshotStarted // the deploy holds the routing lock now
	done := stopAsync(d, e, 1)
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	select {
	case <-done:
		t.Fatal("stop scaled while the deploy held the routing lock")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	await(t, done)
	if got := e.log(); len(got) != 1 || got[0] != "scale" {
		t.Fatalf("events = %v, want the stop's scale only", got)
	}
	if !strings.Contains(st.depError(id), "before it was applied") {
		t.Fatalf("deployment error = %q", st.depError(id))
	}
}

// A deploy already writing its spec is not cancelled: the stop waits for the
// write and then scales what it wrote, and the deploy, seeing the service
// stopped, records that instead of waiting out its convergence timeout and
// alerting.
func TestStopDuringApplyScalesAfterIt(t *testing.T) {
	old := convergeTimeout
	convergeTimeout = time.Minute
	defer func() { convergeTimeout = old }()
	e := newStopEngine()
	e.deployStarted, e.deployRelease = make(chan struct{}), make(chan struct{})
	e.mockEngine.neverConverge = true
	st := newFakeStore(imageApp())
	d := newDeployer(e, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, TriggerManual)
	<-e.deployStarted
	done := stopAsync(d, e, 1)
	time.Sleep(50 * time.Millisecond)
	close(e.deployRelease)
	if got := await(t, done); len(got) != 0 {
		t.Fatalf("an applied deploy was cancelled: %v", got)
	}
	if got := e.log(); strings.Join(got, ",") != "deploy-start,deploy-end,scale" {
		t.Fatalf("events = %v, want the scale after the deploy", got)
	}
	st.SetStatus(t.Context(), 1, StatusIdle)
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	if got := st.depError(id); got != "stopped by alice@example.com before the service converged" {
		t.Fatalf("deployment error = %q", got)
	}
	if st.appStatus(1) != StatusIdle {
		t.Fatalf("app status = %q, want idle", st.appStatus(1))
	}
	if fn.was() {
		t.Fatal("a stopped deploy alerted as failed")
	}
}

// Every deploy kind goes through the same gate: a system deploy (the network
// migration) and a rollback are cancelled like a tenant's.
func TestStopCancelsSystemDeploy(t *testing.T) {
	e := newStopEngine()
	st := newFakeStore(imageApp())
	d := newDeployer(e, &mockBuilder{}, st)
	id := d.EnqueueSystem(t.Context(), 1)
	if got := stopApp(t, d, e, 1); len(got) != 1 || got[0] != id {
		t.Fatalf("cancelled = %v, want [%d]", got, id)
	}
	if len(d.queue) != 0 {
		t.Fatal("stopped system deploy still queued")
	}
	if !strings.Contains(st.depError(id), "before it was applied") {
		t.Fatalf("deployment error = %q", st.depError(id))
	}
}

// Shutting down after a stop must not overwrite the stop's record.
func TestShutdownKeepsStoppedRecord(t *testing.T) {
	e := newStopEngine()
	st := newFakeStore(imageApp())
	d := newDeployer(e, &mockBuilder{}, st)
	id := d.Enqueue(1, TriggerManual)
	stopApp(t, d, e, 1)
	d.Start(context.Background())
	d.Stop()
	if got := st.depError(id); !strings.Contains(got, "before it was applied") {
		t.Fatalf("deployment error = %q", got)
	}
}

// Stops pressed at random moments of a deploy: whatever the timing, the app
// ends with its service scaled to zero after the last spec write, and the
// deployment is recorded exactly once.
func TestStopRaceAlwaysEndsStopped(t *testing.T) {
	shortSettle(t, time.Second)
	for i := range 200 {
		e := newStopEngine()
		e.jitter = true
		st := &countingStore{fakeStore: newFakeStore(imageApp())}
		d := newDeployer(e, &mockBuilder{}, st)
		d.Start(context.Background())
		id := d.Enqueue(1, TriggerManual)
		time.Sleep(time.Duration(rand.IntN(1500)) * time.Microsecond)
		if _, err := d.StopApp(t.Context(), 1, "alice", e.scaler(1)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return st.depStatus(id) != "" })
		d.Stop()
		ev := e.log()
		if len(ev) == 0 || ev[len(ev)-1] != "scale" {
			t.Fatalf("run %d: events %v do not end with the stop's scale", i, ev)
		}
		if n := st.finishes(id); n != 1 {
			t.Fatalf("run %d: deployment recorded %d times", i, n)
		}
	}
}

type countingStore struct {
	*fakeStore
	cmu sync.Mutex
	fin map[int64]int
}

func (c *countingStore) FinishDeployment(ctx context.Context, deployID int64, status, imageTag, errMsg, log string) error {
	c.cmu.Lock()
	if c.fin == nil {
		c.fin = map[int64]int{}
	}
	c.fin[deployID]++
	c.cmu.Unlock()
	return c.fakeStore.FinishDeployment(ctx, deployID, status, imageTag, errMsg, log)
}

func (c *countingStore) finishes(id int64) int {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.fin[id]
}

var _ builder.Builder = (*blockingBuilder)(nil)
var _ io.Writer = (*hubWriter)(nil)

// hookStore lets a test hold or fail the writes a stop races with.
type hookStore struct {
	*fakeStore
	finish func(deployID int64, errMsg string) error // nil: record normally
	create func()                                    // runs inside CreateDeployment
}

func (h *hookStore) FinishDeployment(ctx context.Context, deployID int64, status, imageTag, errMsg, log string) error {
	if h.finish != nil {
		if err := h.finish(deployID, errMsg); err != nil {
			return err
		}
	}
	return h.fakeStore.FinishDeployment(ctx, deployID, status, imageTag, errMsg, log)
}

func (h *hookStore) CreateDeployment(ctx context.Context, appID int64, trigger string) (int64, error) {
	if h.create != nil {
		h.create()
	}
	return h.fakeStore.CreateDeployment(ctx, appID, trigger)
}

// Deploying and stopping an app in a loop while a long build holds the
// worker must not fill the shared queue: a cancelled job leaves it at once.
// Before, each one kept its slot until the worker reached it, while its
// in-flight caps were already free, and 64 of them refused every other
// tenant's deploys with "queue full".
func TestStoppedJobsLeaveTheQueue(t *testing.T) {
	e := newStopEngine()
	bb := &blockingBuilder{started: make(chan struct{})}
	st := newFakeStore(dockerfileApp())
	for _, id := range []int64{3, 4} {
		a := imageApp()
		a.ID = id
		if st.apps == nil {
			st.apps = map[int64]App{}
		}
		st.apps[id] = a
	}
	d := newDeployer(e, bb, st)
	d.Start(context.Background())
	defer d.Stop()
	d.Enqueue(2, TriggerManual)
	<-bb.started // the worker is busy for good

	for i := range 2 * cap(d.queue) {
		if _, err := d.EnqueueErr(3, TriggerManual); err != nil {
			t.Fatalf("deploy %d of the stopped app refused: %v", i, err)
		}
		stopApp(t, d, e, 3)
	}
	if n := len(d.queue); n != 0 {
		t.Fatalf("%d cancelled jobs still hold queue slots", n)
	}
	if _, err := d.EnqueueErr(4, TriggerManual); err != nil {
		t.Fatalf("another app's deploy refused: %v", err)
	}
	d.trackMu.Lock()
	n := len(d.tracked)
	d.trackMu.Unlock()
	if n != 2 { // the build and app 4's deploy
		t.Fatalf("%d deploys still tracked, want 2", n)
	}
}

// A cancellation that cannot be written is not lost: StopApp says so, the
// deploy stays cancelled (it never applies), and the next stop writes it.
func TestStopRetriesAFailedCancellationWrite(t *testing.T) {
	e := newStopEngine()
	fail := true
	var mu sync.Mutex
	st := &hookStore{fakeStore: newFakeStore(imageApp())}
	st.finish = func(int64, string) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("database unavailable")
		}
		return nil
	}
	d := newDeployer(e, &mockBuilder{}, st)
	id := d.Enqueue(1, TriggerManual)
	if _, err := d.StopApp(t.Context(), 1, "alice@example.com", e.scaler(1)); err == nil {
		t.Fatal("a failed write was reported as a successful stop")
	}
	if got := st.depStatus(id); got != "" {
		t.Fatalf("deployment recorded %q despite the failed write", got)
	}
	if e.deploys() != 0 || len(d.queue) != 0 {
		t.Fatal("the cancelled deploy is still queued")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if got := stopApp(t, d, e, 1); len(got) != 1 || got[0] != id {
		t.Fatalf("second stop cancelled %v, want a retry of %d", got, id)
	}
	if st.depError(id) != "stopped by alice@example.com before it was applied" {
		t.Fatalf("deployment error = %q", st.depError(id))
	}
	d.trackMu.Lock()
	n := len(d.tracked)
	d.trackMu.Unlock()
	if n != 0 {
		t.Fatalf("%d deploys still tracked", n)
	}
}

// A stop that fails to scale the service does not mark an applied deploy
// stopped: its later failure is reported and alerted as usual.
func TestFailedStopLeavesAppliedDeployReporting(t *testing.T) {
	old := convergeTimeout
	convergeTimeout = time.Minute
	defer func() { convergeTimeout = old }()
	e := newStopEngine()
	e.deployStarted, e.deployRelease = make(chan struct{}), make(chan struct{})
	e.mockEngine.crashLooping = true
	st := newFakeStore(imageApp())
	d := newDeployer(e, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, TriggerManual)
	<-e.deployStarted
	done := make(chan error, 1)
	go func() {
		_, err := d.StopApp(context.Background(), 1, "alice", func(context.Context) error { return errors.New("swarm unavailable") })
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(e.deployRelease)
	if err := <-done; err == nil {
		t.Fatal("failed scale reported as a stop")
	}
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	waitFor(t, fn.was)
	if st.appStatus(1) != StatusError {
		t.Fatalf("app status = %q, want error", st.appStatus(1))
	}
}

// Shutdown draining the queue and a stop of the same deploy record it once,
// whichever comes first.
func TestShutdownDrainAndStopRecordOnce(t *testing.T) {
	for i := range 200 {
		e := newStopEngine()
		st := &countingStore{fakeStore: newFakeStore(imageApp())}
		d := newDeployer(e, &mockBuilder{}, st)
		id := d.Enqueue(1, TriggerManual)
		j := <-d.queue
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); d.failQueued(j, "server shutting down") }()
		go func() { defer wg.Done(); _, _ = d.StopApp(context.Background(), 1, "alice", e.scaler(1)) }()
		wg.Wait()
		if n := st.finishes(id); n != 1 {
			t.Fatalf("run %d: deployment recorded %d times", i, n)
		}
	}
}

// A deploy recording its outcome when the stop arrives is waited for: its
// app status cannot land after the stop's.
func TestStopWaitsForAFinishingDeploy(t *testing.T) {
	e := newStopEngine()
	hold, entered := make(chan struct{}), make(chan struct{})
	st := &hookStore{fakeStore: newFakeStore(imageApp())}
	st.finish = func(int64, string) error {
		close(entered)
		<-hold
		return nil
	}
	d := newDeployer(e, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()
	id := d.Enqueue(1, TriggerManual)
	<-entered // the deploy converged and is writing its outcome
	done := stopAsync(d, e, 1)
	select {
	case <-done:
		t.Fatal("stop returned while the deploy was still writing its outcome")
	case <-time.After(100 * time.Millisecond):
	}
	close(hold)
	await(t, done)
	if st.appStatus(1) != StatusRunning || st.depStatus(id) != "done" {
		t.Fatalf("deploy outcome %q / app %q not written before the stop returned", st.depStatus(id), st.appStatus(1))
	}
}

// A stop arriving while a deploy is being accepted (its row exists, the
// worker does not know it yet) waits for it and cancels it.
func TestStopWaitsForADeployBeingAccepted(t *testing.T) {
	e := newStopEngine()
	inCreate, release := make(chan struct{}), make(chan struct{})
	st := &hookStore{fakeStore: newFakeStore(imageApp())}
	st.create = func() { close(inCreate); <-release }
	d := newDeployer(e, &mockBuilder{}, st)
	ids := make(chan int64, 1)
	go func() { ids <- d.Enqueue(1, TriggerManual) }()
	<-inCreate
	done := stopAsync(d, e, 1)
	time.Sleep(50 * time.Millisecond)
	close(release)
	id := <-ids
	if got := await(t, done); len(got) != 1 || got[0] != id {
		t.Fatalf("cancelled %v, want [%d]", got, id)
	}
	if len(d.queue) != 0 {
		t.Fatal("the cancelled deploy is still queued")
	}
}

// The network migration may queue a system deploy of a running app that is
// stopped before the deploy runs: it must not start the app again.
func TestSystemDeploySkipsAnAppStoppedMeanwhile(t *testing.T) {
	e := newStopEngine()
	e.mockEngine.stopped = true // the service exists, scaled to zero
	st := newFakeStore(imageApp())
	d := newDeployer(e, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)
	id := d.EnqueueSystem(t.Context(), 1)
	d.run(t.Context(), <-d.queue)
	if e.deploys() != 0 {
		t.Fatal("system deploy started a stopped app")
	}
	if st.depStatus(id) != "error" || !strings.Contains(st.depError(id), "stopped") {
		t.Fatalf("deployment = %q %q", st.depStatus(id), st.depError(id))
	}
	if fn.was() {
		t.Fatal("skipped system deploy alerted")
	}
}
