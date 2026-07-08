package dbservice

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
	mu             sync.Mutex
	pulled         []string
	deployed       []docker.ServiceSpec
	scaled         map[string]uint64
	removed        []string
	removedVolumes []string
	failPull       bool
	removeErr      error // returned by every ServiceRemove call, after recording it
	deployErr      error // returned by every ServiceDeploy call, after recording it
}

func newMockEngine() *mockEngine                                  { return &mockEngine{scaled: map[string]uint64{}} }
func (m *mockEngine) NetworkEnsure(context.Context, string) error { return nil }
func (m *mockEngine) ServiceDeploy(_ context.Context, s docker.ServiceSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deployed = append(m.deployed, s)
	return m.deployErr
}
func (m *mockEngine) ServiceRemove(_ context.Context, n string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, n)
	return m.removeErr
}
func (m *mockEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (m *mockEngine) ServiceProgress(context.Context, string, []string) (docker.ServiceProgress, error) {
	return docker.ServiceProgress{Found: true, Running: 1, Desired: 1}, nil
}
func (m *mockEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	out := map[string]docker.ServiceState{}
	for _, n := range names {
		out[n] = docker.ServiceState{Found: true, Running: 1, Desired: 1}
	}
	return out, nil
}
func (m *mockEngine) ServiceLogs(context.Context, string, bool) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockEngine) ServiceScale(_ context.Context, n string, r uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scaled[n] = r
	return nil
}
func (m *mockEngine) ServiceRestart(context.Context, string) error { return nil }
func (m *mockEngine) VolumeRemove(_ context.Context, n string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removedVolumes = append(m.removedVolumes, n)
	return nil
}
func (m *mockEngine) VolumeArchive(context.Context, string, io.Writer) error      { return nil }
func (m *mockEngine) VolumeRestore(context.Context, string, io.Reader) error      { return nil }
func (m *mockEngine) VolumeChown(context.Context, string, int, int, string) error { return nil }
func (m *mockEngine) ImagePull(_ context.Context, ref string, out io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPull {
		return errors.New("pull boom")
	}
	m.pulled = append(m.pulled, ref)
	out.Write([]byte("pulling " + ref + "\n"))
	return nil
}
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
func (m *mockEngine) Tasks(context.Context) ([]docker.TaskInfo, error)           { return nil, nil }
func (m *mockEngine) NodeSetLabel(context.Context, string, string, string) error { return nil }
func (m *mockEngine) NodeDeleteLabel(context.Context, string, string) error      { return nil }
func (m *mockEngine) ResolveDigest(_ context.Context, ref, _ string) (string, error) {
	return ref, nil
}

type fakeStore struct {
	mu         sync.Mutex
	instance   Instance
	status     map[int64]string
	rowDeleted bool
}

func newFakeStore(inst Instance) *fakeStore {
	return &fakeStore{instance: inst, status: map[int64]string{}}
}
func (f *fakeStore) GetInstance(_ context.Context, id int64) (Instance, error) {
	if f.instance.AppName == "" {
		return Instance{}, errors.New("n/a")
	}
	return f.instance, nil
}
func (f *fakeStore) SetInstanceStatus(_ context.Context, id int64, s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = s
	return nil
}
func (f *fakeStore) DeleteInstanceRow(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowDeleted = true
	return nil
}
func (f *fakeStore) st(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[id]
}

func TestDBNodeConstraint(t *testing.T) {
	if c := dbConstraint(""); c != "node.role==manager" {
		t.Errorf("empty node = %q, want manager", c)
	}
	if c := dbConstraint("worker1"); c != "node.hostname==worker1" {
		t.Errorf("worker node = %q", c)
	}
}

func samplePGInstance() Instance {
	return Instance{ID: 1, Engine: "postgres", AppName: "krill-pg-x", Superuser: "u", SuperuserPassword: "p", Image: "postgres:17"}
}

func newSvc(eng docker.Engine, st Store) *Service {
	return New(eng, st, nil, "krill-net")
}

func TestDeployInstancePullsAndDeploys(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	svc.DeployInstance(1)
	waitFor(t, func() bool { return st.st(1) == "running" })
	if len(eng.pulled) != 1 || eng.pulled[0] != "postgres:17" {
		t.Errorf("pulled = %+v", eng.pulled)
	}
	if len(eng.deployed) != 1 || eng.deployed[0].Name != "krill-pg-x" {
		t.Errorf("deployed = %+v", eng.deployed)
	}
}

func TestDeployInstancePullFailMarksError(t *testing.T) {
	eng := newMockEngine()
	eng.failPull = true
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	svc.DeployInstance(1)
	waitFor(t, func() bool { return st.st(1) == "error" })
	if len(eng.deployed) != 0 {
		t.Error("must not deploy if pull fails")
	}
}

func TestStartStopInstance(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	if err := svc.StartInstance(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if eng.scaled["krill-pg-x"] != 1 {
		t.Errorf("start scale = %d", eng.scaled["krill-pg-x"])
	}
	if st.st(1) != "running" {
		t.Errorf("status after start = %q", st.st(1))
	}
	if err := svc.StopInstance(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if eng.scaled["krill-pg-x"] != 0 {
		t.Errorf("stop scale = %d", eng.scaled["krill-pg-x"])
	}
	if st.st(1) != "idle" {
		t.Errorf("status after stop = %q", st.st(1))
	}
}

func TestDeleteInstanceRemovesService(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	if err := svc.DeleteInstance(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.removed) != 2 || eng.removed[0] != "krill-pg-x" || eng.removed[1] != "krill-dbproxy-1" {
		t.Errorf("removed = %+v", eng.removed)
	}
}

func TestDeleteInstanceKeepsVolumeByDefault(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	if err := svc.DeleteInstance(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.removed) != 2 || eng.removed[0] != "krill-pg-x" || eng.removed[1] != "krill-dbproxy-1" {
		t.Errorf("removed = %+v", eng.removed)
	}
	if len(eng.removedVolumes) != 0 {
		t.Errorf("removedVolumes must be empty by default, got %+v", eng.removedVolumes)
	}
}

func TestDeleteInstanceDestroysVolume(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	if err := svc.DeleteInstance(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	want := volumeName("krill-pg-x")
	if len(eng.removedVolumes) != 1 || eng.removedVolumes[0] != want {
		t.Errorf("removedVolumes = %+v, want [%s]", eng.removedVolumes, want)
	}
}

func sampleRedisInstance() Instance {
	return Instance{ID: 1, Engine: "redis", AppName: "krill-redis-x", SuperuserPassword: "p", Image: "redis:7"}
}

func TestDeleteRedisInstanceKeepsVolumeByDefault(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(sampleRedisInstance())
	svc := newSvc(eng, st)
	if err := svc.DeleteInstance(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.removed) != 2 || eng.removed[0] != "krill-redis-x" || eng.removed[1] != "krill-dbproxy-1" {
		t.Errorf("removed = %+v", eng.removed)
	}
	if len(eng.removedVolumes) != 0 {
		t.Errorf("removedVolumes must be empty by default, got %+v", eng.removedVolumes)
	}
}

func TestDeleteRedisInstanceDestroysVolume(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(sampleRedisInstance())
	svc := newSvc(eng, st)
	if err := svc.DeleteInstance(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	want := volumeName("krill-redis-x")
	if len(eng.removedVolumes) != 1 || eng.removedVolumes[0] != want {
		t.Errorf("removedVolumes = %+v, want [%s]", eng.removedVolumes, want)
	}
}

func TestDeleteInstanceKeepsRowOnServiceRemoveError(t *testing.T) {
	eng := newMockEngine()
	eng.removeErr = errors.New("cannot connect to docker daemon")
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	if err := svc.DeleteInstance(context.Background(), 1, false); err == nil {
		t.Fatal("want error when ServiceRemove fails")
	}
	if st.rowDeleted {
		t.Fatal("row must NOT be deleted when the service removal failed")
	}
}

func TestDeleteInstanceToleratesNotFoundOnServiceRemove(t *testing.T) {
	eng := newMockEngine()
	eng.removeErr = errors.New("no such service: krill-pg-x")
	st := newFakeStore(samplePGInstance())
	svc := newSvc(eng, st)
	if err := svc.DeleteInstance(context.Background(), 1, false); err != nil {
		t.Fatalf("not-found ServiceRemove error must be tolerated, got: %v", err)
	}
	if !st.rowDeleted {
		t.Fatal("row must be deleted when the service removal error is not-found")
	}
	if len(eng.removed) != 2 || eng.removed[0] != "krill-pg-x" || eng.removed[1] != "krill-dbproxy-1" {
		t.Errorf("removed = %+v", eng.removed)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	dl := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(dl) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
