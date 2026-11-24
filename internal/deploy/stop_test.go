package deploy

import (
	"context"
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
	// The slot is free: a new deploy is accepted right away.
	if _, err := d.EnqueueErr(1, TriggerManual); err != nil {
		t.Fatalf("deploy after stop refused: %v", err)
	}
	// The worker drops the stopped job and runs the new one.
	d.run(t.Context(), <-d.queue)
	if e.deploys() != 0 {
		t.Fatalf("stopped queued deploy applied its spec: %v", e.log())
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
	d.run(t.Context(), <-d.queue)
	if e.deploys() != 0 {
		t.Fatalf("stopped system deploy applied: %v", e.log())
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
