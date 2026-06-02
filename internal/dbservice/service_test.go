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
}

func newMockEngine() *mockEngine { return &mockEngine{scaled: map[string]uint64{}} }
func (m *mockEngine) NetworkEnsure(context.Context, string) error { return nil }
func (m *mockEngine) ServiceDeploy(_ context.Context, s docker.ServiceSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deployed = append(m.deployed, s)
	return nil
}
func (m *mockEngine) ServiceRemove(_ context.Context, n string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, n)
	return nil
}
func (m *mockEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
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
func (m *mockEngine) VolumeRemove(_ context.Context, n string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removedVolumes = append(m.removedVolumes, n)
	return nil
}
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
func (m *mockEngine) ServiceUpdateLabels(context.Context, string, map[string]string) error { return nil }

type fakeStore struct {
	mu     sync.Mutex
	pg     PostgresDB
	redis  RedisDB
	status map[int64]string
}

func newFakeStore(pg PostgresDB) *fakeStore { return &fakeStore{pg: pg, status: map[int64]string{}} }
func (f *fakeStore) GetPostgres(_ context.Context, id int64) (PostgresDB, error) { return f.pg, nil }
func (f *fakeStore) GetRedis(_ context.Context, id int64) (RedisDB, error) {
	if f.redis.AppName == "" {
		return RedisDB{}, errors.New("n/a")
	}
	return f.redis, nil
}
func (f *fakeStore) SetPostgresStatus(_ context.Context, id int64, s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = s
	return nil
}
func (f *fakeStore) SetRedisStatus(_ context.Context, id int64, s string) error      { return nil }
func (f *fakeStore) DeletePostgresRow(_ context.Context, id int64) error             { return nil }
func (f *fakeStore) DeleteRedisRow(_ context.Context, id int64) error                { return nil }
func (f *fakeStore) st(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[id]
}

func samplePG() PostgresDB {
	return PostgresDB{ID: 1, AppName: "krill-pg-x", DatabaseName: "a", DatabaseUser: "u", DatabasePassword: "p", Image: "postgres:17"}
}

func newSvc(eng docker.Engine, st Store) *Service {
	return New(eng, st, nil, "krill-net")
}

func TestDeployPostgresPullsAndDeploys(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePG())
	svc := newSvc(eng, st)
	svc.DeployPostgres(context.Background(), 1)
	waitFor(t, func() bool { return st.st(1) == "running" })
	if len(eng.pulled) != 1 || eng.pulled[0] != "postgres:17" {
		t.Errorf("pulled = %+v", eng.pulled)
	}
	if len(eng.deployed) != 1 || eng.deployed[0].Name != "krill-pg-x" {
		t.Errorf("deployed = %+v", eng.deployed)
	}
}

func TestDeployPostgresPullFailMarksError(t *testing.T) {
	eng := newMockEngine()
	eng.failPull = true
	st := newFakeStore(samplePG())
	svc := newSvc(eng, st)
	svc.DeployPostgres(context.Background(), 1)
	waitFor(t, func() bool { return st.st(1) == "error" })
	if len(eng.deployed) != 0 {
		t.Error("must not deploy if pull fails")
	}
}

func TestStartStopPostgres(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePG())
	svc := newSvc(eng, st)
	if err := svc.StartPostgres(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if eng.scaled["krill-pg-x"] != 1 {
		t.Errorf("start scale = %d", eng.scaled["krill-pg-x"])
	}
	if st.st(1) != "running" {
		t.Errorf("status after start = %q", st.st(1))
	}
	if err := svc.StopPostgres(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if eng.scaled["krill-pg-x"] != 0 {
		t.Errorf("stop scale = %d", eng.scaled["krill-pg-x"])
	}
	if st.st(1) != "idle" {
		t.Errorf("status after stop = %q", st.st(1))
	}
}

func TestDeletePostgresRemovesService(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePG())
	svc := newSvc(eng, st)
	if err := svc.DeletePostgres(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.removed) != 1 || eng.removed[0] != "krill-pg-x" {
		t.Errorf("removed = %+v", eng.removed)
	}
}

func TestDeletePostgresKeepsVolumeByDefault(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePG())
	svc := newSvc(eng, st)
	if err := svc.DeletePostgres(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.removed) != 1 || eng.removed[0] != "krill-pg-x" {
		t.Errorf("removed = %+v", eng.removed)
	}
	if len(eng.removedVolumes) != 0 {
		t.Errorf("removedVolumes must be empty by default, got %+v", eng.removedVolumes)
	}
}

func TestDeletePostgresDestroysVolume(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePG())
	svc := newSvc(eng, st)
	if err := svc.DeletePostgres(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	want := volumeName("krill-pg-x")
	if len(eng.removedVolumes) != 1 || eng.removedVolumes[0] != want {
		t.Errorf("removedVolumes = %+v, want [%s]", eng.removedVolumes, want)
	}
}

func sampleRedis() RedisDB {
	return RedisDB{ID: 1, AppName: "krill-redis-x", Password: "p", Image: "redis:7"}
}

func TestDeleteRedisKeepsVolumeByDefault(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePG())
	st.redis = sampleRedis()
	svc := newSvc(eng, st)
	if err := svc.DeleteRedis(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.removed) != 1 || eng.removed[0] != "krill-redis-x" {
		t.Errorf("removed = %+v", eng.removed)
	}
	if len(eng.removedVolumes) != 0 {
		t.Errorf("removedVolumes must be empty by default, got %+v", eng.removedVolumes)
	}
}

func TestDeleteRedisDestroysVolume(t *testing.T) {
	eng := newMockEngine()
	st := newFakeStore(samplePG())
	st.redis = sampleRedis()
	svc := newSvc(eng, st)
	if err := svc.DeleteRedis(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	want := volumeName("krill-redis-x")
	if len(eng.removedVolumes) != 1 || eng.removedVolumes[0] != want {
		t.Errorf("removedVolumes = %+v, want [%s]", eng.removedVolumes, want)
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
