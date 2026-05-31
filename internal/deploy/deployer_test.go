package deploy

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

type mockEngine struct {
	mu       sync.Mutex
	deployed []docker.ServiceSpec
	failNext bool
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
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (m *mockEngine) ServiceLogs(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, nil
}

type fakeStore struct {
	mu     sync.Mutex
	app    App
	status map[int64]string
}

func newFakeStore(a App) *fakeStore { return &fakeStore{app: a, status: map[int64]string{}} }
func (f *fakeStore) GetApplication(_ context.Context, id int64) (App, error) {
	return f.app, nil
}
func (f *fakeStore) SetStatus(_ context.Context, id int64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = status
	return nil
}
func (f *fakeStore) get(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[id]
}

func sampleApp() App {
	return App{ID: 1, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.127-0-0-1.sslip.io", Port: 80, Env: map[string]string{"K": "V"}}
}

func TestBuildSpec(t *testing.T) {
	d := New(&mockEngine{}, newFakeStore(sampleApp()), "krill-net")
	spec := d.buildSpec(sampleApp())
	if spec.Name != "krill-web" {
		t.Errorf("name = %q", spec.Name)
	}
	if spec.Image != "nginx:alpine" {
		t.Errorf("image = %q", spec.Image)
	}
	if spec.Labels["traefik.enable"] != "true" {
		t.Error("missing traefik labels")
	}
	if spec.Network != "krill-net" || spec.Replicas != 1 {
		t.Error("network/replicas wrong")
	}
}

func TestWorkerSetsRunning(t *testing.T) {
	eng := &mockEngine{}
	st := newFakeStore(sampleApp())
	d := New(eng, st, "krill-net")
	d.Start(context.Background())
	defer d.Stop()

	d.Enqueue(1)

	deadline := time.Now().Add(2 * time.Second)
	for st.get(1) != StatusRunning {
		if time.Now().After(deadline) {
			t.Fatalf("status never became running: %q", st.get(1))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(eng.deployed) != 1 {
		t.Fatalf("expected 1 deploy, got %d", len(eng.deployed))
	}
}

func TestWorkerSetsErrorOnFailure(t *testing.T) {
	eng := &mockEngine{failNext: true}
	st := newFakeStore(sampleApp())
	d := New(eng, st, "krill-net")
	d.Start(context.Background())
	defer d.Stop()

	d.Enqueue(1)

	deadline := time.Now().Add(2 * time.Second)
	for st.get(1) != StatusError {
		if time.Now().After(deadline) {
			t.Fatalf("status never became error: %q", st.get(1))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEnqueueAfterStopNoPanic(t *testing.T) {
	d := New(&mockEngine{}, newFakeStore(sampleApp()), "krill-net")
	d.Start(context.Background())
	d.Stop()
	// Должно не паниковать и просто игнорироваться.
	d.Enqueue(1)
	d.Enqueue(1)
}

func TestStopIdempotent(t *testing.T) {
	d := New(&mockEngine{}, newFakeStore(sampleApp()), "krill-net")
	d.Start(context.Background())
	d.Stop()
	d.Stop() // повторный Stop не должен паниковать
}
