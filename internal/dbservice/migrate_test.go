package dbservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
)

type migEngine struct {
	mockEngine
	mu     sync.Mutex
	calls  []string
	state  docker.ServiceState    // response for ServiceState before stop
	tasks  []docker.TaskPlacement // running tasks of the instance
	nodes  []docker.SwarmNode
	exists map[string]bool // key nodeID+"/"+vol
	failAt string          // method name that should fail
}

func (m *migEngine) rec(call string) error {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	if m.failAt == strings.SplitN(call, ":", 2)[0] {
		return errors.New("injected: " + call)
	}
	return nil
}

func (m *migEngine) ServiceState(_ context.Context, name string) (docker.ServiceState, error) {
	// After a recorded scale(0) report stopped; after scale(1)/deploy report running.
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state
	for _, c := range m.calls {
		if c == "ServiceScale:"+name+":0" {
			st = docker.ServiceState{Found: true, Running: 0, Desired: 0}
		}
		if c == "ServiceScale:"+name+":1" || strings.HasPrefix(c, "ServiceDeploy:") {
			st = docker.ServiceState{Found: true, Running: 1, Desired: 1}
		}
	}
	return st, nil
}
func (m *migEngine) Nodes(context.Context) ([]docker.SwarmNode, error) { return m.nodes, nil }
func (m *migEngine) ServiceTasks(context.Context, string) ([]docker.TaskPlacement, error) {
	return m.tasks, nil
}
func (m *migEngine) ServiceScale(_ context.Context, name string, n uint64) error {
	return m.rec(fmt.Sprintf("ServiceScale:%s:%d", name, n))
}
func (m *migEngine) ServiceDeploy(_ context.Context, spec docker.ServiceSpec) error {
	return m.rec("ServiceDeploy:" + spec.Name)
}
func (m *migEngine) ImagePull(_ context.Context, ref string, _ io.Writer) error {
	return m.rec("ImagePull:" + ref)
}
func (m *migEngine) VolumeExistsOn(_ context.Context, name, node string) (bool, error) {
	if err := m.rec("VolumeExistsOn:" + node + "/" + name); err != nil {
		return false, err
	}
	return m.exists[node+"/"+name], nil
}
func (m *migEngine) VolumeRemoveOn(_ context.Context, name, node string) error {
	return m.rec("VolumeRemoveOn:" + node + "/" + name)
}
func (m *migEngine) VolumeArchive(_ context.Context, name string, out io.Writer, node string) error {
	if err := m.rec("VolumeArchive:" + node + "/" + name); err != nil {
		return err
	}
	_, _ = out.Write([]byte("tar-bytes"))
	return nil
}
func (m *migEngine) VolumeRestore(_ context.Context, name string, in io.Reader, node string) error {
	if err := m.rec("VolumeRestore:" + node + "/" + name); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, in)
	return nil
}

type migStore struct {
	inst     Instance
	statuses []string
	nodeSets []string
}

func (st *migStore) GetInstance(context.Context, int64) (Instance, error) { return st.inst, nil }
func (st *migStore) SetInstanceStatus(_ context.Context, _ int64, s string) error {
	st.statuses = append(st.statuses, s)
	return nil
}
func (st *migStore) SetInstanceNode(_ context.Context, _ int64, h string) error {
	st.nodeSets = append(st.nodeSets, h)
	st.inst.NodeHostname = h
	return nil
}
func (st *migStore) DeleteInstanceRow(context.Context, int64) error { return nil }

func migFixture(failAt string) (*Service, *migEngine, *migStore) {
	eng := &migEngine{
		state:  docker.ServiceState{Found: true, Running: 1, Desired: 1},
		nodes:  []docker.SwarmNode{{ID: "mgr", Hostname: "cp", Leader: true}, {ID: "w1", Hostname: "worker-1"}},
		tasks:  []docker.TaskPlacement{{NodeID: "mgr", NodeName: "cp", State: "running"}},
		exists: map[string]bool{"mgr/krill-pg-a-data": true},
		failAt: failAt,
	}
	st := &migStore{inst: Instance{ID: 7, AppName: "krill-pg-a", Engine: "postgres", Image: "postgres:17", Status: "running", NodeHostname: ""}}
	return New(eng, st, nil, "krill-net"), eng, st
}

func TestMigrateHappyHot(t *testing.T) {
	s, eng, st := migFixture("")
	if err := s.migrateInstanceNode(context.Background(), 7, "worker-1", false); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := []string{
		"ServiceScale:krill-pg-a:0",
		"VolumeExistsOn:w1/krill-pg-a-data",
		"VolumeArchive:mgr/krill-pg-a-data",
		"VolumeRestore:w1/krill-pg-a-data",
	}
	got := strings.Join(eng.calls, "|")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in calls %v", w, eng.calls)
		}
	}
	// VolumeExistsOn(src) runs BEFORE the stop; archive strictly after scale 0.
	if idx(eng.calls, "VolumeArchive:mgr/krill-pg-a-data") < idx(eng.calls, "ServiceScale:krill-pg-a:0") {
		t.Fatal("archived before the service was stopped")
	}
	if len(st.nodeSets) != 1 || st.nodeSets[0] != "worker-1" {
		t.Fatalf("node writes = %v", st.nodeSets)
	}
	if last(st.statuses) != "running" {
		t.Fatalf("final status %q", last(st.statuses))
	}
	if strings.Contains(got, "VolumeRemoveOn:mgr/") {
		t.Fatal("source volume removed without deleteSource")
	}
}

func TestMigrateDeleteSource(t *testing.T) {
	s, eng, _ := migFixture("")
	if err := s.migrateInstanceNode(context.Background(), 7, "worker-1", true); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !slices.Contains(eng.calls, "VolumeRemoveOn:mgr/krill-pg-a-data") {
		t.Fatalf("source volume not removed: %v", eng.calls)
	}
}

func TestMigratePreCleansStaleTarget(t *testing.T) {
	s, eng, _ := migFixture("")
	eng.exists["w1/krill-pg-a-data"] = true
	if err := s.migrateInstanceNode(context.Background(), 7, "worker-1", false); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if idx(eng.calls, "VolumeRemoveOn:w1/krill-pg-a-data") > idx(eng.calls, "VolumeRestore:w1/krill-pg-a-data") {
		t.Fatal("stale target volume not removed before restore")
	}
}

// Invariant: a copy failure → metadata is NOT written, the service comes back
// up on the source, and the source volume is not removed.
func TestMigrateRollbackOnCopyFailure(t *testing.T) {
	for _, failAt := range []string{"VolumeArchive", "VolumeRestore"} {
		s, eng, st := migFixture(failAt)
		if err := s.migrateInstanceNode(context.Background(), 7, "worker-1", true); err == nil {
			t.Fatalf("%s: expected error", failAt)
		}
		if len(st.nodeSets) != 0 {
			t.Fatalf("%s: metadata written on failure: %v", failAt, st.nodeSets)
		}
		if !slices.Contains(eng.calls, "ServiceScale:krill-pg-a:1") {
			t.Fatalf("%s: service not scaled back on source", failAt)
		}
		if slices.Contains(eng.calls, "VolumeRemoveOn:mgr/krill-pg-a-data") {
			t.Fatalf("%s: SOURCE volume removed on failure", failAt)
		}
		if last(st.statuses) != "running" {
			t.Fatalf("%s: final status %q", failAt, last(st.statuses))
		}
	}
}

// A target-deploy failure AFTER the metadata write → metadata is reverted and
// the instance is redeployed on the source.
func TestMigrateRevertOnTargetDeployFailure(t *testing.T) {
	s, _, st := migFixture("ServiceDeploy")
	if err := s.migrateInstanceNode(context.Background(), 7, "worker-1", false); err == nil {
		t.Fatal("expected error")
	}
	if len(st.nodeSets) < 2 || st.nodeSets[len(st.nodeSets)-1] != "" {
		t.Fatalf("metadata not reverted to source: %v", st.nodeSets)
	}
}

func TestMigrateColdSkipsStopAndDeploy(t *testing.T) {
	s, eng, st := migFixture("")
	eng.state = docker.ServiceState{Found: true, Running: 0, Desired: 0}
	st.inst.Status = "idle"
	if err := s.migrateInstanceNode(context.Background(), 7, "worker-1", false); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got := strings.Join(eng.calls, "|")
	if strings.Contains(got, "ServiceScale") || strings.Contains(got, "ServiceDeploy") {
		t.Fatalf("cold migration touched the service: %v", eng.calls)
	}
	if last(st.statuses) != "idle" {
		t.Fatalf("final status %q", last(st.statuses))
	}
}

func TestMigrateGuards(t *testing.T) {
	s, _, _ := migFixture("")
	// busy
	if !oplock.TryAcquire(oplock.DBInstance("krill-pg-a")) {
		t.Fatal("setup lock")
	}
	if err := s.migrateInstanceNode(context.Background(), 7, "worker-1", false); !errors.Is(err, ErrInstanceBusy) {
		t.Fatalf("want ErrInstanceBusy, got %v", err)
	}
	oplock.Release(oplock.DBInstance("krill-pg-a"))
	// same node (metadata "" resolves to the leader; the task also runs there)
	if err := s.migrateInstanceNode(context.Background(), 7, "", false); !errors.Is(err, ErrSameNode) {
		t.Fatalf("want ErrSameNode, got %v", err)
	}
	// unknown node
	if err := s.migrateInstanceNode(context.Background(), 7, "ghost", false); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("want ErrNodeNotFound, got %v", err)
	}
	// missing source volume
	s2, eng2, _ := migFixture("")
	eng2.exists = map[string]bool{}
	if err := s2.migrateInstanceNode(context.Background(), 7, "worker-1", false); !errors.Is(err, ErrSourceVolume) {
		t.Fatalf("want ErrSourceVolume, got %v", err)
	}
}

func idx(calls []string, s string) int {
	for i, c := range calls {
		if c == s {
			return i
		}
	}
	return 1 << 30
}

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}
