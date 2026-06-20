package deploy

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

type mockEngine struct {
	mu               sync.Mutex
	deployed         []docker.ServiceSpec
	failNext         bool
	neverConverge    bool
	partialRunning   bool
	crashLooping     bool
	rollingBack      bool // swarm rolled the update back (FailureAction=Rollback)
	updateInProgress bool // StartFirst update: only the OLD task is running
}

func (m *mockEngine) NetworkEnsure(context.Context, string) error { return nil }
func (m *mockEngine) ServiceDeploy(_ context.Context, s docker.ServiceSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		return errors.New("boom")
	}
	m.deployed = append(m.deployed, s)
	return nil
}
func (m *mockEngine) ServiceRemove(context.Context, string) error { return nil }
func (m *mockEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	if m.crashLooping {
		return docker.ServiceState{Found: true, Running: 0, Desired: 1, Failed: 5}, nil
	}
	if m.neverConverge {
		return docker.ServiceState{Found: true, Running: 0, Desired: 1}, nil
	}
	if m.partialRunning {
		return docker.ServiceState{Found: true, Running: 1, Desired: 2}, nil
	}
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}

// ServiceProgress mirrors ServiceState but in baseline-relative terms: Running
// counts only NEW (non-baseline) tasks. updateInProgress models the situation
// the convergence fix targets — the old StartFirst task still running while
// the new one starts (ServiceState would report Running=1 here).
func (m *mockEngine) ServiceProgress(context.Context, string, []string) (docker.ServiceProgress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.rollingBack:
		return docker.ServiceProgress{Found: true, Desired: 1, Running: 0, UpdateState: "rollback_started", TaskIDs: []string{"t-old"}}, nil
	case m.updateInProgress:
		return docker.ServiceProgress{Found: true, Desired: 1, Running: 0, UpdateState: "updating", TaskIDs: []string{"t-old"}}, nil
	case m.crashLooping:
		return docker.ServiceProgress{Found: true, Desired: 1, Running: 0, Failed: 5}, nil
	case m.neverConverge:
		return docker.ServiceProgress{Found: true, Desired: 1, Running: 0}, nil
	case m.partialRunning:
		return docker.ServiceProgress{Found: true, Desired: 2, Running: 1, TaskIDs: []string{"t-new"}}, nil
	}
	return docker.ServiceProgress{Found: true, Desired: 1, Running: 1, TaskIDs: []string{"t-new"}}, nil
}

func (m *mockEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	out := map[string]docker.ServiceState{}
	for _, n := range names {
		st, _ := m.ServiceState(context.Background(), n)
		out[n] = st
	}
	return out, nil
}
func (m *mockEngine) ServiceLogs(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockEngine) ServiceScale(context.Context, string, uint64) error       { return nil }
func (m *mockEngine) ServiceRestart(context.Context, string) error             { return nil }
func (m *mockEngine) VolumeRemove(context.Context, string) error               { return nil }
func (m *mockEngine) VolumeArchive(context.Context, string, io.Writer) error   { return nil }
func (m *mockEngine) VolumeRestore(context.Context, string, io.Reader) error   { return nil }
func (m *mockEngine) ImagePull(_ context.Context, _ string, _ io.Writer) error { return nil }
func (m *mockEngine) ServiceUpdateLabels(context.Context, string, map[string]string) error {
	return nil
}
func (m *mockEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return nil
}
func (m *mockEngine) ExecInteractive(context.Context, string, []string) (docker.ExecSession, error) {
	return nil, errors.New("exec not supported")
}
func (m *mockEngine) RegistryCheck(context.Context, string, string, string) error { return nil }
func (m *mockEngine) ListContainerStats(context.Context) ([]docker.ContainerStat, error) {
	return nil, nil
}
func (m *mockEngine) NodeInfo(context.Context) (docker.NodeInfo, error) {
	return docker.NodeInfo{}, nil
}
func (m *mockEngine) Nodes(context.Context) ([]docker.SwarmNode, error)         { return nil, nil }
func (m *mockEngine) NodeSetAvailability(context.Context, string, string) error { return nil }
func (m *mockEngine) NodeRemove(context.Context, string, bool) error            { return nil }
func (m *mockEngine) SwarmWorkerToken(context.Context) (string, error)          { return "", nil }
func (m *mockEngine) ServiceTasks(context.Context, string) ([]docker.TaskPlacement, error) {
	return nil, nil
}
func (m *mockEngine) NodeSetLabel(context.Context, string, string, string) error { return nil }
func (m *mockEngine) NodeDeleteLabel(context.Context, string, string) error      { return nil }

type mockBuilder struct {
	mu     sync.Mutex
	called bool
	fail   bool
}

func (b *mockBuilder) Build(_ context.Context, req builder.BuildRequest, out io.Writer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called = true
	out.Write([]byte("building " + req.ImageTag + "\n"))
	if b.fail {
		return errors.New("build boom")
	}
	return nil
}
func (b *mockBuilder) wasCalled() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.called }

type fakeStore struct {
	mu      sync.Mutex
	app     App
	status  map[int64]string
	deploys map[int64]string // deployID -> final status
	nextID  int64
	depApp  map[int64]int64 // deployID -> appID
}

func newFakeStore(a App) *fakeStore {
	return &fakeStore{app: a, status: map[int64]string{}, deploys: map[int64]string{}, depApp: map[int64]int64{}, nextID: 100}
}
func (f *fakeStore) GetApplication(_ context.Context, id int64) (App, error) { return f.app, nil }
func (f *fakeStore) GetDeploymentApp(_ context.Context, deployID int64) (App, error) {
	return f.app, nil
}
func (f *fakeStore) SetStatus(_ context.Context, id int64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = status
	return nil
}
func (f *fakeStore) CreateDeployment(_ context.Context, appID int64, trigger string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.depApp[f.nextID] = appID
	return f.nextID, nil
}
func (f *fakeStore) FinishDeployment(_ context.Context, deployID int64, status, imageTag, errMsg, log string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploys[deployID] = status
	return nil
}
func (f *fakeStore) appStatus(id int64) string { f.mu.Lock(); defer f.mu.Unlock(); return f.status[id] }
func (f *fakeStore) depStatus(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deploys[id]
}

func imageApp() App {
	return App{ID: 1, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.127-0-0-1.sslip.io", Port: 80, Env: map[string]string{"K": "V"}, SourceType: "image",
		Domains: []traefik.Domain{{Host: "web.127-0-0-1.sslip.io", TLS: false, Exposed: true}}}
}
func dockerfileApp() App {
	return App{ID: 2, Name: "api", Domain: "api.x", Port: 3000, Env: map[string]string{},
		SourceType: "dockerfile", GitURL: "https://github.com/x/y.git", GitBranch: "main", DockerfilePath: "Dockerfile",
		Domains: []traefik.Domain{{Host: "api.x", TLS: false, Exposed: true}}}
}

func newDeployer(eng docker.Engine, b builder.Builder, st Store) *Deployer {
	return New(eng, b, st, NewLogHub(), "krill-net")
}

func TestBuildSpecPlacement(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	base := imageApp() // ID 1

	if sp := d.buildSpec(base, "i"); sp.Global || sp.SpreadNodeID || len(sp.Constraints) != 0 {
		t.Errorf("any: unexpected placement %+v", sp)
	}

	pin := base
	pin.PlacementMode = "pin"
	pin.PlacementNodes = []string{"n1", "n2"}
	sp := d.buildSpec(pin, "i")
	if sp.Global || !sp.SpreadNodeID {
		t.Errorf("pin: want spread not global; got global=%v spread=%v", sp.Global, sp.SpreadNodeID)
	}
	if len(sp.Constraints) != 1 || sp.Constraints[0] != "node.labels.krill.place.1==1" {
		t.Errorf("pin constraints = %v", sp.Constraints)
	}

	g := base
	g.PlacementMode = "global"
	g.PlacementNodes = []string{"n1"}
	if sp := d.buildSpec(g, "i"); !sp.Global || sp.SpreadNodeID {
		t.Errorf("global: want Global not spread; got global=%v spread=%v", sp.Global, sp.SpreadNodeID)
	}

	empty := base
	empty.PlacementMode = "pin" // no nodes selected -> no constraint
	if sp := d.buildSpec(empty, "i"); len(sp.Constraints) != 0 {
		t.Errorf("pin with empty node set: unexpected constraint %v", sp.Constraints)
	}
}

func TestBuildSpecImage(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	spec := d.buildSpec(imageApp(), "nginx:alpine")
	if spec.Name != "krill-1" || spec.Image != "nginx:alpine" {
		t.Errorf("spec = %+v", spec)
	}
	if spec.Labels["traefik.enable"] != "true" || spec.Network != "krill-net" {
		t.Error("labels/network wrong")
	}
}

func TestBuildSpecAdvanced(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	app := imageApp()
	app.MemoryLimitBytes = 268435456
	app.NanoCPUs = 500000000
	app.RestartCondition = "on-failure"
	app.RestartMaxAttempts = 3
	app.Healthcheck = &docker.HealthcheckSpec{
		Test:     []string{"CMD-SHELL", "true"},
		Interval: 30 * time.Second,
		Timeout:  5 * time.Second,
		Retries:  3,
	}

	spec := d.buildSpec(app, "nginx:alpine")
	if spec.MemoryLimitBytes != 268435456 {
		t.Errorf("MemoryLimitBytes = %d, want 268435456", spec.MemoryLimitBytes)
	}
	if spec.NanoCPUs != 500000000 {
		t.Errorf("NanoCPUs = %d, want 500000000", spec.NanoCPUs)
	}
	if spec.RestartCondition != "on-failure" {
		t.Errorf("RestartCondition = %q, want on-failure", spec.RestartCondition)
	}
	if spec.RestartMaxAttempts != 3 {
		t.Errorf("RestartMaxAttempts = %d, want 3", spec.RestartMaxAttempts)
	}
	if spec.Healthcheck == nil {
		t.Fatal("Healthcheck = nil, want non-nil")
	}
	if spec.Healthcheck.Retries != 3 {
		t.Errorf("Healthcheck.Retries = %d, want 3", spec.Healthcheck.Retries)
	}
}

func TestImageDeployNoBuild(t *testing.T) {
	eng := &mockEngine{}
	b := &mockBuilder{}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, b, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	if b.wasCalled() {
		t.Error("builder must NOT be called for image source")
	}
	if len(eng.deployed) != 1 || eng.deployed[0].Image != "nginx:alpine" {
		t.Fatalf("deployed = %+v", eng.deployed)
	}
	if st.appStatus(1) != StatusRunning {
		t.Errorf("app status = %q", st.appStatus(1))
	}
}

func TestDockerfileDeployBuilds(t *testing.T) {
	eng := &mockEngine{}
	b := &mockBuilder{}
	st := newFakeStore(dockerfileApp())
	d := newDeployer(eng, b, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(2, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	if !b.wasCalled() {
		t.Error("builder MUST be called for dockerfile source")
	}
	want := "krill-2:" + strconv.FormatInt(id, 10)
	if len(eng.deployed) != 1 || eng.deployed[0].Image != want {
		t.Fatalf("deployed image = %+v (want %s)", eng.deployed, want)
	}
}

func TestBuildFailureMarksError(t *testing.T) {
	eng := &mockEngine{}
	b := &mockBuilder{fail: true}
	st := newFakeStore(dockerfileApp())
	d := newDeployer(eng, b, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(2, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	if len(eng.deployed) != 0 {
		t.Error("must NOT deploy when build fails")
	}
	if st.appStatus(2) != StatusError {
		t.Errorf("app status = %q", st.appStatus(2))
	}
}

func TestEnqueueAfterStopNoPanic(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	d.Start(context.Background())
	d.Stop()
	if id := d.Enqueue(1, "manual"); id != 0 {
		t.Errorf("Enqueue after Stop must return 0, got %d", id)
	}
}

func TestStopIdempotent(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	d.Start(context.Background())
	d.Stop()
	d.Stop()
}

type blockingBuilder struct{ started chan struct{} }

func (b *blockingBuilder) Build(ctx context.Context, _ builder.BuildRequest, _ io.Writer) error {
	close(b.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestStopCancelsInFlightBuild(t *testing.T) {
	bb := &blockingBuilder{started: make(chan struct{})}
	st := newFakeStore(dockerfileApp())
	d := newDeployer(&mockEngine{}, bb, st)
	d.Start(context.Background())
	d.Enqueue(2, "manual")
	<-bb.started // build is now in flight
	done := make(chan struct{})
	go func() { d.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel the in-flight build promptly")
	}
}

func TestDeployConvergeTimeoutMarksError(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 200*time.Millisecond, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	eng := &mockEngine{neverConverge: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	if st.appStatus(1) != StatusError {
		t.Errorf("app status = %q, want error", st.appStatus(1))
	}
}

func TestBuildSpecMounts(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	app := imageApp()
	app.Mounts = []docker.MountSpec{
		{Type: "volume", Source: "krill-vol-1-data", Target: "/data"},
	}
	spec := d.buildSpec(app, "nginx:alpine")
	if len(spec.Mounts) != 1 {
		t.Fatalf("expected 1 mount threaded into ServiceSpec, got %d", len(spec.Mounts))
	}
	m := spec.Mounts[0]
	if m.Type != "volume" || m.Source != "krill-vol-1-data" || m.Target != "/data" {
		t.Fatalf("mount not threaded verbatim: %+v", m)
	}
}

func TestBuildSpecRegistryAuth(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	app := imageApp()
	app.RegistryAuth = "ABC123"
	if spec := d.buildSpec(app, "nginx:alpine"); spec.RegistryAuth != "ABC123" {
		t.Fatalf("RegistryAuth = %q, want ABC123", spec.RegistryAuth)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBuildSpecCommandArgs(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	app := imageApp()
	app.Args = []string{"start-dev"}
	spec := d.buildSpec(app, "keycloak:latest")
	if len(spec.Args) != 1 || spec.Args[0] != "start-dev" {
		t.Errorf("spec.Args = %v, want [start-dev]", spec.Args)
	}
}

// A service that is partially up at the deadline (e.g. a slow JVM still warming
// up) must NOT be recorded as a hard error — it should land as "deploying" so
// the live status poll reconciles it to running.
func TestDeploySlowStartMarksDeployingNotError(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 200*time.Millisecond, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	if got := st.appStatus(1); got != StatusDeploying {
		t.Errorf("app status = %q, want %q (slow start, not error)", got, StatusDeploying)
	}
}

// Stop must (a) cancel the in-flight job's context so shutdown doesn't hang for
// the full job timeout, and (b) drain queued-but-unstarted jobs, failing their
// deployment rows instead of orphaning them as 'running' forever.
func TestStopCancelsInflightAndDrainsQueue(t *testing.T) {
	bb := &blockingBuilder{started: make(chan struct{})}
	st := newFakeStore(dockerfileApp())
	d := newDeployer(&mockEngine{}, bb, st)
	d.Start(context.Background())

	id1 := d.Enqueue(2, "manual") // becomes in-flight, blocks in Build
	<-bb.started
	id2 := d.Enqueue(2, "manual") // sits in the queue behind id1

	done := make(chan struct{})
	go func() { d.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung — in-flight job context was not canceled")
	}

	if got := st.depStatus(id1); got != "error" {
		t.Errorf("in-flight deployment %d status = %q, want error", id1, got)
	}
	if got := st.depStatus(id2); got != "error" {
		t.Errorf("queued deployment %d status = %q, want error (must be drained, not orphaned)", id2, got)
	}
}

// A full queue must reject the deployment immediately (deployment row marked
// error) instead of blocking the calling HTTP handler goroutine for hours.
func TestEnqueueQueueFullRejectsImmediately(t *testing.T) {
	eng := &mockEngine{}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	// No Start(): nothing drains the queue, so it fills at its capacity.

	var lastID int64
	for i := 0; i < cap(d.queue); i++ {
		if lastID = d.Enqueue(1, "manual"); lastID == 0 {
			t.Fatalf("enqueue %d rejected before the queue was full", i)
		}
	}

	done := make(chan int64, 1)
	go func() { done <- d.Enqueue(1, "manual") }()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("over-capacity enqueue returned %d, want 0 (rejected)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("over-capacity enqueue blocked — must reject immediately")
	}
}

// A rolling update that swarm rolled back (FailureAction=Rollback) must be a
// FAILED deploy — before the fix the old task satisfied Running>=Desired and
// the rollback was reported as instant success.
func TestDeployRollbackMarksError(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 2*time.Second, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	eng := &mockEngine{rollingBack: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	if got := st.appStatus(1); got != StatusError {
		t.Errorf("app status = %q, want error (update rolled back)", got)
	}
}

// During a StartFirst rolling update only the OLD task is running until the
// new one becomes ready. The deploy must NOT be declared converged on that old
// task (the pre-fix behavior): it should land as "deploying" via the
// still-starting path, never as an instant success.
func TestDeployUpdateDoesNotConvergeOnOldTask(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 200*time.Millisecond, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	eng := &mockEngine{updateInProgress: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	if got := st.appStatus(1); got != StatusDeploying {
		t.Errorf("app status = %q, want %q (update in flight must not read as converged)", got, StatusDeploying)
	}
}

// A crash-looping service (tasks keep failing) must be marked error fast — and
// NOT treated as "still starting".
func TestDeployCrashLoopMarksError(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 2*time.Second, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	eng := &mockEngine{crashLooping: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	if got := st.appStatus(1); got != StatusError {
		t.Errorf("app status = %q, want error (crash-loop)", got)
	}
}

type fakeNotifier struct {
	mu     sync.Mutex
	appID  int64
	reason string
	called bool
}

func (f *fakeNotifier) DeployFailed(_ context.Context, appID int64, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.appID = appID
	f.reason = reason
}
func (f *fakeNotifier) was() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.called }

func TestDeployFailureNotifies(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 2*time.Second, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	eng := &mockEngine{crashLooping: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	waitFor(t, func() bool { return fn.was() })
	if fn.appID != 1 {
		t.Fatalf("notify appID = %d, want 1", fn.appID)
	}
}

// SetConvergeTimeout overrides the package default.
func TestSetConvergeTimeout(t *testing.T) {
	old := convergeTimeout
	defer func() { convergeTimeout = old }()
	SetConvergeTimeout(42 * time.Second)
	if convergeTimeout != 42*time.Second {
		t.Errorf("convergeTimeout = %v, want 42s", convergeTimeout)
	}
	SetConvergeTimeout(0) // 0 = keep current
	if convergeTimeout != 42*time.Second {
		t.Errorf("convergeTimeout after 0 = %v, want unchanged 42s", convergeTimeout)
	}
}

func TestBuildSpecPublishesRawPorts(t *testing.T) {
	d := &Deployer{network: "krill-net"}
	app := App{
		ID: 7, Image: "img:1", Domain: "x.example", Port: 8080,
		Ports: []docker.PortSpec{
			{Target: 22, Published: 2222, Mode: "host"},
			{Target: 53, Published: 5353, Mode: "host", UDP: true},
		},
	}
	spec := d.buildSpec(app, "img:1")
	if len(spec.Ports) != 2 {
		t.Fatalf("spec.Ports = %d, want 2", len(spec.Ports))
	}
	if spec.Ports[0].Published != 2222 || spec.Ports[0].Target != 22 || spec.Ports[0].Mode != "host" {
		t.Fatalf("tcp port = %+v", spec.Ports[0])
	}
	if !spec.Ports[1].UDP || spec.Ports[1].Published != 5353 {
		t.Fatalf("udp port = %+v", spec.Ports[1])
	}
}
