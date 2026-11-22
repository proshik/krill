package deploy

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// digestMockEngine is a minimal Engine fake used by TestImageDeployPinsDigest.
// It only handles ResolveDigest and ServiceDeploy (+ ServiceProgress for
// convergence); all other methods panic or return zero values.
type digestMockEngine struct {
	digest    string
	digestErr error
	deployed  []docker.ServiceSpec
	mu        sync.Mutex
}

func (f *digestMockEngine) ResolveDigest(_ context.Context, ref, _ string) (string, error) {
	if f.digestErr != nil {
		return "", f.digestErr
	}
	return f.digest, nil
}
func (f *digestMockEngine) ServiceDeploy(_ context.Context, s docker.ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployed = append(f.deployed, s)
	return nil
}
func (f *digestMockEngine) ServiceProgress(context.Context, string, []string) (docker.ServiceProgress, error) {
	return docker.ServiceProgress{Found: true, Desired: 1, Running: 1, TaskIDs: []string{"t1"}}, nil
}
func (f *digestMockEngine) NetworkEnsure(context.Context, string) (bool, error) { return false, nil }
func (f *digestMockEngine) NetworkRemove(context.Context, string) error         { return nil }
func (f *digestMockEngine) ServiceRemove(context.Context, string) error         { return nil }
func (f *digestMockEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (f *digestMockEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	m := map[string]docker.ServiceState{}
	for _, n := range names {
		m[n] = docker.ServiceState{Found: true, Running: 1, Desired: 1}
	}
	return m, nil
}
func (f *digestMockEngine) ServiceLogs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return nil, nil
}
func (f *digestMockEngine) ServiceScale(context.Context, string, uint64) error { return nil }
func (f *digestMockEngine) ServiceRestart(context.Context, string) error       { return nil }
func (f *digestMockEngine) VolumeRemove(context.Context, string) error         { return nil }
func (f *digestMockEngine) VolumeArchive(context.Context, string, io.Writer, string) error {
	return nil
}
func (f *digestMockEngine) VolumeRestore(context.Context, string, io.Reader, string) error {
	return nil
}
func (f *digestMockEngine) VolumeRemoveOn(context.Context, string, string) error { return nil }
func (f *digestMockEngine) VolumeExistsOn(context.Context, string, string) (bool, error) {
	return false, nil
}
func (f *digestMockEngine) VolumeChown(context.Context, string, int, int, string) error { return nil }
func (f *digestMockEngine) ImagePull(context.Context, string, io.Writer) error          { return nil }
func (f *digestMockEngine) ServiceUpdateLabels(context.Context, string, map[string]string) error {
	return nil
}
func (f *digestMockEngine) ServiceLabels(context.Context, string) (map[string]string, bool, error) {
	return nil, false, nil
}
func (f *digestMockEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return nil
}
func (f *digestMockEngine) ExecInteractive(context.Context, string, []string) (docker.ExecSession, error) {
	return nil, errors.New("not supported")
}
func (f *digestMockEngine) RegistryCheck(context.Context, string, string, string) error { return nil }
func (f *digestMockEngine) ListContainerStats(context.Context) ([]docker.ContainerStat, error) {
	return nil, nil
}
func (f *digestMockEngine) NodeInfo(context.Context) (docker.NodeInfo, error) {
	return docker.NodeInfo{}, nil
}
func (f *digestMockEngine) Nodes(context.Context) ([]docker.SwarmNode, error)         { return nil, nil }
func (f *digestMockEngine) NodeSetAvailability(context.Context, string, string) error { return nil }
func (f *digestMockEngine) NodeRemove(context.Context, string, bool) error            { return nil }
func (f *digestMockEngine) SwarmWorkerToken(context.Context) (string, error)          { return "", nil }
func (f *digestMockEngine) ServiceTasks(context.Context, string) ([]docker.TaskPlacement, error) {
	return nil, nil
}
func (f *digestMockEngine) Tasks(context.Context) ([]docker.TaskInfo, error)           { return nil, nil }
func (f *digestMockEngine) NodeSetLabel(context.Context, string, string, string) error { return nil }
func (f *digestMockEngine) NodeDeleteLabel(context.Context, string, string) error      { return nil }

type mockEngine struct {
	mu               sync.Mutex
	deployed         []docker.ServiceSpec
	failNext         bool
	neverConverge    bool
	partialRunning   bool
	crashLooping     bool
	rollingBack      bool            // swarm rolled the update back (FailureAction=Rollback)
	updateInProgress bool            // StartFirst update: only the OLD task is running
	stopped          bool            // the service was scaled to zero
	globalPending    bool            // a global service with no new task yet (no eligible node)
	slow             map[string]bool // services still starting (partial), by name

	baselineSnapshotFails bool // first ServiceProgress (the pre-deploy snapshot) errors
	baselineAsked         bool

	chownCalls []chownCall
	chownErr   error
}

type chownCall struct {
	vol      string
	uid, gid int
	node     string
}

func (m *mockEngine) NetworkEnsure(context.Context, string) (bool, error) { return false, nil }
func (m *mockEngine) NetworkRemove(context.Context, string) error         { return nil }
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
func (m *mockEngine) ServiceProgress(_ context.Context, name string, baseline []string) (docker.ServiceProgress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return docker.ServiceProgress{Found: true, Desired: 0, TaskIDs: []string{"t-new"}}, nil
	}
	if m.globalPending {
		return docker.ServiceProgress{Found: true, Desired: 0, Global: true}, nil
	}
	if m.slow[name] {
		return docker.ServiceProgress{Found: true, Desired: 2, Running: 1, TaskIDs: []string{"t-new"}}, nil
	}
	// baselineSnapshotFails models a transient failure of the pre-deploy task
	// snapshot, then answers baseline-relative like the real engine: the old
	// task "t-old" counts as new only when the baseline is missing — which is
	// exactly how a swallowed snapshot error turns into instant false success.
	if m.baselineSnapshotFails {
		if !m.baselineAsked {
			m.baselineAsked = true
			return docker.ServiceProgress{}, errors.New("task list unavailable")
		}
		for _, id := range baseline {
			if id == "t-old" {
				return docker.ServiceProgress{Found: true, Desired: 1, Running: 0, UpdateState: "updating", TaskIDs: []string{"t-old"}}, nil
			}
		}
		return docker.ServiceProgress{Found: true, Desired: 1, Running: 1, TaskIDs: []string{"t-old"}}, nil
	}
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
func (m *mockEngine) ServiceLogs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockEngine) ServiceScale(context.Context, string, uint64) error { return nil }
func (m *mockEngine) ServiceRestart(context.Context, string) error       { return nil }
func (m *mockEngine) VolumeRemove(context.Context, string) error         { return nil }
func (m *mockEngine) VolumeArchive(context.Context, string, io.Writer, string) error {
	return nil
}
func (m *mockEngine) VolumeRestore(context.Context, string, io.Reader, string) error {
	return nil
}
func (m *mockEngine) VolumeRemoveOn(context.Context, string, string) error { return nil }
func (m *mockEngine) VolumeExistsOn(context.Context, string, string) (bool, error) {
	return false, nil
}
func (m *mockEngine) VolumeChown(_ context.Context, vol string, uid, gid int, node string) error {
	m.chownCalls = append(m.chownCalls, chownCall{vol, uid, gid, node})
	return m.chownErr
}
func (m *mockEngine) ImagePull(_ context.Context, _ string, _ io.Writer) error { return nil }
func (m *mockEngine) ServiceUpdateLabels(context.Context, string, map[string]string) error {
	return nil
}
func (m *mockEngine) ServiceLabels(context.Context, string) (map[string]string, bool, error) {
	return nil, false, nil
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
func (m *mockEngine) Tasks(context.Context) ([]docker.TaskInfo, error)           { return nil, nil }
func (m *mockEngine) NodeSetLabel(context.Context, string, string, string) error { return nil }
func (m *mockEngine) NodeDeleteLabel(context.Context, string, string) error      { return nil }
func (m *mockEngine) ResolveDigest(_ context.Context, ref, _ string) (string, error) {
	return ref, nil // tests don't hit a registry; pass the ref through unchanged
}

type mockBuilder struct {
	mu     sync.Mutex
	called bool
	fail   bool
	commit string // reported as the built commit
}

func (b *mockBuilder) Build(_ context.Context, req builder.BuildRequest, out io.Writer) (builder.BuildResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.called = true
	out.Write([]byte("building " + req.ImageTag + "\n"))
	if b.fail {
		return builder.BuildResult{}, errors.New("build boom")
	}
	return builder.BuildResult{Commit: b.commit}, nil
}
func (b *mockBuilder) wasCalled() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.called }

type fakeStore struct {
	depErr  map[int64]string // deployment id -> recorded error message
	apps    map[int64]App    // extra apps by id; the rest resolve to app
	failGet bool             // GetDeploymentApp fails (a deploy failing while preparing)
	mu      sync.Mutex
	app     App
	status  map[int64]string
	deploys map[int64]string // deployID -> final status
	nextID  int64
	depApp  map[int64]int64 // deployID -> appID

	runningByOrg int64 // stubbed CountRunningDeploymentsByOrg result
	orgCountErr  error // stubbed CountRunningDeploymentsByOrg error

	triggers  map[int64]string // deployID -> trigger
	setImages []ImageRef       // SetApplicationImage calls, in order
}

func newFakeStore(a App) *fakeStore {
	return &fakeStore{app: a, status: map[int64]string{}, deploys: map[int64]string{}, depApp: map[int64]int64{}, nextID: 100}
}
func (f *fakeStore) GetApplication(_ context.Context, id int64) (App, error) { return f.app, nil }
func (f *fakeStore) GetDeploymentApp(_ context.Context, deployID int64, ref ImageRef) (App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return App{}, errors.New("prepare failed")
	}
	a := f.app
	if extra, ok := f.apps[f.depApp[deployID]]; ok {
		a = extra
	}
	if ref.Tag != "" && a.SourceType != "dockerfile" {
		a.Image, a.Tag = ref.Image, ref.Tag
	}
	return a, nil
}
func (f *fakeStore) SetApplicationImage(_ context.Context, _ int64, image, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setImages = append(f.setImages, ImageRef{Image: image, Tag: tag})
	f.app.Image, f.app.Tag = image, tag
	return nil
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
	if f.triggers == nil {
		f.triggers = map[int64]string{}
	}
	f.triggers[f.nextID] = trigger
	return f.nextID, nil
}

// CountRunningDeployments mirrors the real store: a deployment is in flight
// once created and until finished. A fake that always answered 0 would leave
// the enqueue guard untested and would hide the behaviour change from every
// test that deploys the same app twice.
func (f *fakeStore) CountRunningDeployments(_ context.Context, appID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for depID, a := range f.depApp {
		if a != appID {
			continue
		}
		if _, finished := f.deploys[depID]; !finished {
			n++
		}
	}
	return n, nil
}

// CountRunningDeploymentsByOrg is a plain stub (unlike CountRunningDeployments,
// which derives its answer from depApp/deploys): the per-org cap test wants to
// set the in-flight count directly, independent of what per-app bookkeeping
// happens to hold.
func (f *fakeStore) CountRunningDeploymentsByOrg(_ context.Context, appID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.orgCountErr != nil {
		return 0, f.orgCountErr
	}
	return f.runningByOrg, nil
}

func (f *fakeStore) FinishDeployment(_ context.Context, deployID int64, status, imageTag, errMsg, log string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploys[deployID] = status
	if f.depErr == nil {
		f.depErr = map[int64]string{}
	}
	f.depErr[deployID] = errMsg
	return nil
}
func (f *fakeStore) depError(id int64) string { f.mu.Lock(); defer f.mu.Unlock(); return f.depErr[id] }
func (f *fakeStore) deploymentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.depApp)
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

func (b *blockingBuilder) Build(ctx context.Context, _ builder.BuildRequest, _ io.Writer) (builder.BuildResult, error) {
	close(b.started)
	<-ctx.Done()
	return builder.BuildResult{}, ctx.Err()
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

// A service with no task running and no failure (a first deploy still pulling
// its image: Swarm reports no update status for a new service) fails only
// after the background wait, as a convergence timeout.
func TestDeployConvergeTimeoutMarksError(t *testing.T) {
	shortSettle(t, 300*time.Millisecond)
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
	if got := st.depError(id); got != "timed out waiting for the service to converge" {
		t.Errorf("error = %q", got)
	}
}

// The case seen in production: a first deploy whose image takes longer to
// pull than KRILL_CONVERGE_TIMEOUT. It used to be recorded as failed while the
// app came up moments later; now it is done once its task runs.
func TestFirstDeploySlowPullBecomesDone(t *testing.T) {
	shortSettle(t, 5*time.Second)
	eng := &mockEngine{neverConverge: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond)
	if got := st.depStatus(id); got == "done" || got == "error" {
		t.Fatalf("status past the deadline = %q, want still in progress", got)
	}
	eng.mu.Lock()
	eng.neverConverge = false
	eng.mu.Unlock()
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
}

// Stopping the app while its deploy is still converging ends the watch at
// once: the deployment is closed as stopped, the app's status is left to the
// stop, the slot is free for the next deploy and no failure is alerted.
func TestStopWhileSettlingFreesTheApp(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	n := &fakeNotifier{}
	d := newDeployer(eng, &mockBuilder{}, st)
	d.SetNotifier(n)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond)
	st.SetStatus(context.Background(), 1, StatusIdle) // what the Stop handler records
	eng.mu.Lock()
	eng.stopped = true
	eng.mu.Unlock()
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	if got := st.depError(id); got != "stopped before the service converged" {
		t.Fatalf("error = %q", got)
	}
	if got := st.appStatus(1); got != StatusIdle {
		t.Fatalf("app status = %q, want the stop's %q", got, StatusIdle)
	}
	if n.was() {
		t.Fatal("a stop during the wait was alerted as a failed deploy")
	}
}

// A system deploy (the organization network migration) can start for an app
// whose earlier deploy is still converging; the earlier watch is superseded
// rather than confirming whatever the service converges to.
func TestSystemDeploySupersedesSettlingWatch(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	n := &fakeNotifier{}
	d := newDeployer(eng, &mockBuilder{}, st)
	d.SetNotifier(n)
	d.Start(context.Background())
	defer d.Stop()

	first := d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond)
	second := d.EnqueueSystem(context.Background(), 1)
	waitFor(t, func() bool { return st.depStatus(first) == "error" })
	if got := st.depError(first); got != errSuperseded.Error() {
		t.Fatalf("first deploy error = %q", got)
	}
	eng.setPartial(false)
	waitFor(t, func() bool { return st.depStatus(second) == "done" })
	for _, s := range n.successes() {
		if s == first {
			t.Fatal("the superseded deploy was reported successful")
		}
	}
}

// While one app's deploy settles in the background, the build worker is free:
// another app deploys and finishes meanwhile.
func TestSettlingDoesNotHoldTheWorker(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{slow: map[string]bool{docker.ServiceName(1): true}}
	st := newFakeStore(imageApp())
	other := imageApp()
	other.ID, other.Name = 2, "other"
	st.apps = map[int64]App{2: other}
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	slow := d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond)
	fast := d.Enqueue(2, "manual")
	waitFor(t, func() bool { return st.depStatus(fast) == "done" })
	if got := st.depStatus(slow); got == "done" || got == "error" {
		t.Fatalf("the settling deploy finished as %q; the test no longer overlaps them", got)
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

func (m *mockEngine) setPartial(v bool) { m.mu.Lock(); m.partialRunning = v; m.mu.Unlock() }

func shortSettle(t *testing.T, settle time.Duration) {
	t.Helper()
	oldT, oldP, oldS, oldSP := convergeTimeout, convergePollInterval, settleTimeout, settlePollInterval
	convergeTimeout, convergePollInterval = 100*time.Millisecond, 10*time.Millisecond
	settleTimeout, settlePollInterval = settle, 10*time.Millisecond
	t.Cleanup(func() {
		convergeTimeout, convergePollInterval, settleTimeout, settlePollInterval = oldT, oldP, oldS, oldSP
	})
}

// A service still starting at the deadline (a large image still pulling) is
// not done: the deployment stays running until Swarm actually converges, and
// only then becomes done with the app running. Before the fix it was recorded
// done at the deadline while the old version was still serving.
func TestDeploySlowStartStaysRunningUntilConverged(t *testing.T) {
	shortSettle(t, 5*time.Second)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	time.Sleep(400 * time.Millisecond) // well past convergeTimeout
	if got := st.depStatus(id); got == "done" || got == "error" {
		t.Fatalf("deployment status past the deadline = %q, want still in progress (not converged yet)", got)
	}
	if got := st.appStatus(1); got != StatusDeploying {
		t.Fatalf("app status = %q, want %q", got, StatusDeploying)
	}
	if again := d.Enqueue(1, "manual"); again != 0 {
		t.Fatalf("a second deploy was accepted while the first is still converging")
	}
	eng.setPartial(false)
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	if got := st.appStatus(1); got != StatusRunning {
		t.Fatalf("app status after convergence = %q, want running", got)
	}
}

// A service that never converges, even in the background, is a failed deploy
// that says it timed out waiting — not a success and not a silent "done".
func TestDeploySlowStartTimesOutHonestly(t *testing.T) {
	shortSettle(t, 300*time.Millisecond)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) != "running" && st.depStatus(id) != "" })
	if got := st.depStatus(id); got != "error" {
		t.Fatalf("deployment status = %q, want error", got)
	}
	if got := st.depError(id); got != "timed out waiting for the service to converge" {
		t.Fatalf("error message = %q", got)
	}
}

// Stopping Krill while a deploy settles in the background must not hang Stop
// and must not leave the deployment looking finished: it is recorded as
// interrupted (error), the app left deploying for the status poll.
func TestStopDuringSettleRecordsInterrupted(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())

	id := d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond)
	done := make(chan struct{})
	go func() { d.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung on a settling deploy")
	}
	if got := st.depStatus(id); got != "error" {
		t.Fatalf("deployment status = %q, want error", got)
	}
	if got := st.depError(id); !strings.Contains(got, "interrupted") {
		t.Fatalf("error message = %q, want interrupted", got)
	}
	if got := st.appStatus(1); got != StatusDeploying {
		t.Fatalf("app status = %q, want deploying", got)
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
	// A different app: one app may have only one deploy in flight, so queueing
	// behind id1 requires a second app.
	id2 := d.Enqueue(3, "manual") // sits in the queue behind id1

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

	// One app per slot: a single app is now capped at one in-flight deploy, so
	// filling the shared queue takes as many distinct apps as it has slots.
	// That cap is the point — it is what stops one caller from starving every
	// other tenant — so this test fills the queue the only way that remains.
	var lastID int64
	for i := 0; i < cap(d.queue); i++ {
		if lastID = d.Enqueue(int64(i+1), "manual"); lastID == 0 {
			t.Fatalf("enqueue %d rejected before the queue was full", i)
		}
	}

	done := make(chan int64, 1)
	go func() { done <- d.Enqueue(int64(cap(d.queue)+1), "manual") }()
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
// task (the pre-fix behavior): it keeps waiting via the still-starting path,
// never an instant success.
func TestDeployUpdateDoesNotConvergeOnOldTask(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{updateInProgress: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	time.Sleep(400 * time.Millisecond)
	if got := st.depStatus(id); got == "done" || got == "error" {
		t.Fatalf("deployment status = %q, want still in progress", got)
	}
	if got := st.appStatus(1); got != StatusDeploying {
		t.Errorf("app status = %q, want %q (update in flight must not read as converged)", got, StatusDeploying)
	}
}

// The pre-deploy task snapshot is what keeps a rolling update from reporting
// instant success on the old StartFirst task. Its error was swallowed, leaving
// the baseline empty — silently restoring the very bug the baseline exists to
// prevent: the deploy reports "done" while the old version is still serving.
// A snapshot that cannot be taken must fail the deploy instead.
func TestDeployBaselineSnapshotFailureFailsDeploy(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 200*time.Millisecond, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	eng := &mockEngine{baselineSnapshotFails: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	// fakeStore records a deployment's status only when it finishes, so wait for
	// a terminal value rather than for "not running".
	waitFor(t, func() bool { s := st.depStatus(id); return s == "done" || s == "error" })
	if got := st.depStatus(id); got != "error" {
		t.Errorf("deployment status = %q, want %q (a lost baseline must not report success)", got, "error")
	}
	if got := st.appStatus(1); got == StatusRunning {
		t.Errorf("app status = %q: deploy reported success without converging", got)
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
	mu        sync.Mutex
	appID     int64
	reason    string
	called    bool
	succeeded []int64 // deployment ids
}

func (f *fakeNotifier) DeploySucceeded(_ context.Context, deploymentID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeeded = append(f.succeeded, deploymentID)
}
func (f *fakeNotifier) successes() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.succeeded...)
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

// A deploy that converges reports its deployment as succeeded — and only
// that: a failed one must not.
func TestDeploySuccessNotifies(t *testing.T) {
	st := newFakeStore(imageApp())
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
	waitFor(t, func() bool { return len(fn.successes()) == 1 })
	if got := fn.successes(); got[0] != id {
		t.Fatalf("DeploySucceeded(%v), want deployment %d", got, id)
	}
	if fn.was() {
		t.Fatal("a successful deploy also reported a failure")
	}
}

func TestDeployFailureDoesNotReportSuccess(t *testing.T) {
	oldT, oldP := convergeTimeout, convergePollInterval
	convergeTimeout, convergePollInterval = 2*time.Second, 20*time.Millisecond
	defer func() { convergeTimeout, convergePollInterval = oldT, oldP }()

	st := newFakeStore(imageApp())
	d := newDeployer(&mockEngine{crashLooping: true}, &mockBuilder{}, st)
	fn := &fakeNotifier{}
	d.SetNotifier(fn)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "error" })
	waitFor(t, func() bool { return fn.was() })
	if got := fn.successes(); len(got) != 0 {
		t.Fatalf("a failed deploy reported success: %v", got)
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

// TestImageDeployPinsDigest verifies that resolveImageRef returns the
// registry-resolved digest-pinned reference on success, and falls back to the
// plain tag when the engine returns an error.
func TestImageDeployPinsDigest(t *testing.T) {
	const pinnedDigest = "docker.io/library/nginx@sha256:abc123"

	// Success path: engine resolves the digest; resolveImageRef must return it.
	fe := &digestMockEngine{digest: pinnedDigest}
	app := App{ID: 1, Name: "web", Image: "nginx", Tag: "latest", SourceType: "image"}
	ref := resolveImageRef(context.Background(), fe, app, "nginx:latest")
	if ref != pinnedDigest {
		t.Fatalf("resolveImageRef = %q, want pinned digest %q", ref, pinnedDigest)
	}

	// Error path: engine cannot reach registry; resolveImageRef must fall back to the plain tag.
	feErr := &digestMockEngine{digestErr: errors.New("no registry")}
	fallback := resolveImageRef(context.Background(), feErr, app, "nginx:latest")
	if fallback != "nginx:latest" {
		t.Fatalf("fallback resolveImageRef = %q, want plain tag %q", fallback, "nginx:latest")
	}
}

// TestImageDeployUsesDigest verifies that a full image deploy (via Enqueue)
// passes the digest-pinned image reference to ServiceDeploy when ResolveDigest
// succeeds, and falls back to the plain tag on error.
func TestImageDeployUsesDigest(t *testing.T) {
	const pinnedDigest = "docker.io/library/nginx@sha256:abc123"

	// Success path: deployer should pin to digest.
	eng := &digestMockEngine{digest: pinnedDigest}
	st := newFakeStore(imageApp()) // imageApp(): Image="nginx", Tag="alpine", SourceType="image"
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	waitFor(t, func() bool { return st.depStatus(id) == "done" })

	eng.mu.Lock()
	deployed := eng.deployed
	eng.mu.Unlock()

	if len(deployed) != 1 {
		t.Fatalf("expected 1 deploy, got %d", len(deployed))
	}
	if deployed[0].Image != pinnedDigest {
		t.Fatalf("deployed image = %q, want pinned digest %q", deployed[0].Image, pinnedDigest)
	}

	// Error path: engine ResolveDigest fails → deployer falls back to plain tag.
	eng2 := &digestMockEngine{digestErr: errors.New("registry unreachable")}
	st2 := newFakeStore(imageApp())
	d2 := newDeployer(eng2, &mockBuilder{}, st2)
	d2.Start(context.Background())
	defer d2.Stop()

	id2 := d2.Enqueue(1, "manual")
	waitFor(t, func() bool { return st2.depStatus(id2) == "done" })

	eng2.mu.Lock()
	deployed2 := eng2.deployed
	eng2.mu.Unlock()

	if len(deployed2) != 1 {
		t.Fatalf("expected 1 deploy on fallback, got %d", len(deployed2))
	}
	const wantPlain = "nginx:alpine"
	if deployed2[0].Image != wantPlain {
		t.Fatalf("fallback deployed image = %q, want %q", deployed2[0].Image, wantPlain)
	}
}

func TestChownOwnedVolumes(t *testing.T) {
	m := &mockEngine{}
	d := &Deployer{engine: m}
	app := App{
		PlacementMode:  "pin",
		PlacementNodes: []string{"node-x"},
		Mounts: []docker.MountSpec{
			{Type: "volume", Source: "krill-vol-1-data", Target: "/data", Owner: "1000:0"},
			{Type: "volume", Source: "krill-vol-1-plain", Target: "/x"}, // no owner → skipped
		},
	}
	if err := d.chownOwnedVolumes(context.Background(), app, io.Discard); err != nil {
		t.Fatalf("chownOwnedVolumes: %v", err)
	}
	if len(m.chownCalls) != 1 {
		t.Fatalf("want 1 chown call, got %d (%+v)", len(m.chownCalls), m.chownCalls)
	}
	if got := m.chownCalls[0]; got != (chownCall{"krill-vol-1-data", 1000, 0, "node-x"}) {
		t.Fatalf("chown call = %+v", got)
	}
}

func TestChownOwnedVolumesFailsDeploy(t *testing.T) {
	m := &mockEngine{chownErr: errors.New("boom")}
	d := &Deployer{engine: m}
	app := App{Mounts: []docker.MountSpec{{Source: "v", Owner: "1000:1000"}}}
	if err := d.chownOwnedVolumes(context.Background(), app, io.Discard); err == nil {
		t.Fatal("want error when VolumeChown fails")
	}
}

// One application may have only one deploy in flight. The queue is 64 deep,
// drained by a single worker and shared by every tenant, so without this a
// caller retrying one app in a loop — an agent is the obvious way — fills the
// queue and every other tenant's deploys start failing with "queue full".
func TestEnqueueRefusesASecondDeployForTheSameApp(t *testing.T) {
	st := newFakeStore(imageApp())
	d := newDeployer(&mockEngine{}, &mockBuilder{}, st)
	// No Start(): nothing drains the queue, so the first deploy stays in flight.

	first := d.Enqueue(1, "manual")
	if first == 0 {
		t.Fatal("first deploy was rejected")
	}
	if second := d.Enqueue(1, "manual"); second != 0 {
		t.Fatalf("second deploy for the same app returned %d, want 0 (rejected)", second)
	}
	// A different app is unaffected: the cap is per application, not global.
	if other := d.Enqueue(2, "manual"); other == 0 {
		t.Fatal("a deploy for a different app was rejected")
	}
	// Once the first finishes, the app can deploy again.
	if err := st.FinishDeployment(context.Background(), first, "done", "", "", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if again := d.Enqueue(1, "manual"); again == 0 {
		t.Fatal("app could not deploy again after its in-flight deploy finished")
	}
}

func TestBuildSpecContainerLabels(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	app := imageApp()
	app.ContainerLabels = map[string]string{"krill.app": "web"}
	spec := d.buildSpec(app, "nginx:alpine")
	if spec.ContainerLabels["krill.app"] != "web" {
		t.Errorf("container labels = %v", spec.ContainerLabels)
	}
	if spec.Labels["krill.app"] != "" {
		t.Error("container labels must not become service labels")
	}
}

// slowCountStore widens the window between enqueue's in-flight count and its
// create, where two overlapping requests used to both count zero.
type slowCountStore struct{ *fakeStore }

func (s slowCountStore) CountRunningDeployments(ctx context.Context, appID int64) (int64, error) {
	n, err := s.fakeStore.CountRunningDeployments(ctx, appID)
	time.Sleep(50 * time.Millisecond) // the answer is already stale when it arrives
	return n, err
}

// A double click on Deploy sends two overlapping requests; only one may queue.
func TestEnqueueConcurrentRequestsQueueOneDeploy(t *testing.T) {
	st := newFakeStore(imageApp())
	d := newDeployer(&mockEngine{}, &mockBuilder{}, slowCountStore{st})
	ids := make(chan int64, 2)
	for i := 0; i < 2; i++ {
		go func() { ids <- d.Enqueue(1, "manual") }()
	}
	queued := 0
	for i := 0; i < 2; i++ {
		if <-ids != 0 {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("overlapping deploys of one app queued %d, want 1", queued)
	}
}

// A global service whose new tasks cannot be placed yet (its only eligible
// node is down) reports zero desired tasks; that is waiting, not a stop.
func TestGlobalServiceWithoutEligibleNodeKeepsWaiting(t *testing.T) {
	shortSettle(t, 5*time.Second)
	eng := &mockEngine{globalPending: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	id := d.Enqueue(1, "manual")
	time.Sleep(400 * time.Millisecond)
	if got := st.depStatus(id); got == "done" || got == "error" {
		t.Fatalf("global service without a node yet ended as %q (%s)", got, st.depError(id))
	}
	eng.mu.Lock()
	eng.globalPending = false
	eng.mu.Unlock()
	waitFor(t, func() bool { return st.depStatus(id) == "done" })
}

// A system deploy that fails before it replaces the service leaves the
// earlier rollout's watch alone: that rollout still finishes as its own.
func TestFailedSystemDeployDoesNotSupersedeTheWatch(t *testing.T) {
	shortSettle(t, time.Minute)
	eng := &mockEngine{partialRunning: true}
	st := newFakeStore(imageApp())
	d := newDeployer(eng, &mockBuilder{}, st)
	d.Start(context.Background())
	defer d.Stop()

	first := d.Enqueue(1, "manual")
	time.Sleep(300 * time.Millisecond)
	st.mu.Lock()
	st.failGet = true
	st.mu.Unlock()
	second := d.EnqueueSystem(context.Background(), 1)
	waitFor(t, func() bool { return st.depStatus(second) == "error" })
	if got := st.depStatus(first); got == "done" || got == "error" {
		t.Fatalf("the earlier rollout's watch ended as %q (%s)", got, st.depError(first))
	}
	eng.setPartial(false)
	waitFor(t, func() bool { return st.depStatus(first) == "done" })
}
