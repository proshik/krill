package deploy

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

type mockEngine struct {
	mu        sync.Mutex
	deployed  []docker.ServiceSpec
	deployErr error
}

func (m *mockEngine) NetworkEnsure(context.Context, string) error { return nil }
func (m *mockEngine) ServiceDeploy(_ context.Context, s docker.ServiceSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deployed = append(m.deployed, s)
	return m.deployErr
}
func (m *mockEngine) ServiceRemove(context.Context, string) error { return nil }
func (m *mockEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{}, nil
}
func (m *mockEngine) ServiceLogs(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, nil
}

type fakeStore struct {
	mu     sync.Mutex
	app    App
	status map[int64]string
}

func (f *fakeStore) GetApplication(_ context.Context, id int64) (App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.app, nil
}
func (f *fakeStore) SetStatus(_ context.Context, id int64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		f.status = map[int64]string{}
	}
	f.status[id] = status
	return nil
}

func TestBuildSpec(t *testing.T) {
	d := &Deployer{network: "krill-net", baseDomain: "127-0-0-1.sslip.io"}
	app := App{
		ID:     1,
		Name:   "web",
		Image:  "nginx",
		Tag:    "alpine",
		Domain: "web.127-0-0-1.sslip.io",
		Port:   80,
		Env:    map[string]string{"FOO": "bar"},
	}
	spec := d.buildSpec(app)

	if spec.Name != "krill-web" {
		t.Errorf("name = %q, want krill-web", spec.Name)
	}
	if spec.Image != "nginx:alpine" {
		t.Errorf("image = %q, want nginx:alpine", spec.Image)
	}
	if spec.Labels["traefik.enable"] != "true" {
		t.Error("traefik not enabled")
	}
	if spec.Env["FOO"] != "bar" {
		t.Error("env not propagated")
	}
	if spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", spec.Replicas)
	}
	if spec.Network != "krill-net" {
		t.Errorf("network = %q", spec.Network)
	}
}

func TestWorkerSetsRunning(t *testing.T) {
	eng := &mockEngine{}
	store := &fakeStore{app: App{
		ID: 1, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.127-0-0-1.sslip.io", Port: 80,
	}}
	d := New(eng, store, "krill-net", "127-0-0-1.sslip.io")
	d.Start(1)

	d.Enqueue(1)

	deadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		st := store.status[1]
		store.mu.Unlock()
		if st == StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never became running: %q", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.Stop()

	eng.mu.Lock()
	n := len(eng.deployed)
	eng.mu.Unlock()
	if n != 1 {
		t.Errorf("expected 1 deploy, got %d", n)
	}
}

func TestWorkerSetsErrorOnFailure(t *testing.T) {
	eng := &mockEngine{deployErr: io.ErrUnexpectedEOF}
	store := &fakeStore{app: App{
		ID: 1, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.127-0-0-1.sslip.io", Port: 80,
	}}
	d := New(eng, store, "krill-net", "127-0-0-1.sslip.io")
	d.Start(1)
	defer d.Stop()

	d.Enqueue(1)

	deadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		st := store.status[1]
		store.mu.Unlock()
		if st == StatusError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never became error: %q", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
