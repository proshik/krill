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
	mu            sync.Mutex
	deployed      []docker.ServiceSpec
	failNext      bool
	neverConverge bool
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
	if m.neverConverge {
		return docker.ServiceState{Found: true, Running: 0, Desired: 1}, nil
	}
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (m *mockEngine) ServiceLogs(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockEngine) ServiceScale(context.Context, string, uint64) error                        { return nil }
func (m *mockEngine) ServiceRestart(context.Context, string) error                              { return nil }
func (m *mockEngine) VolumeRemove(context.Context, string) error                               { return nil }
func (m *mockEngine) ImagePull(_ context.Context, _ string, _ io.Writer) error                 { return nil }
func (m *mockEngine) ServiceUpdateLabels(context.Context, string, map[string]string) error     { return nil }
func (m *mockEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return nil
}
func (m *mockEngine) RegistryCheck(context.Context, string, string, string) error { return nil }

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
func (f *fakeStore) depStatus(id int64) string { f.mu.Lock(); defer f.mu.Unlock(); return f.deploys[id] }

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
