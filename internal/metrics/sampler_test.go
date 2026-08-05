package metrics_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
)

type capStore struct {
	mu         sync.Mutex
	ins        []string // "node/comp"
	caps       map[string]int
	prunes     int      // count of Prune calls
	keptExcept []string // last keep set passed to PruneCapacityExcept
}

func (s *capStore) Insert(ctx context.Context, node, comp string, cpu float64, mem, lim int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ins = append(s.ins, node+"/"+comp)
	return nil
}
func (s *capStore) UpsertCapacity(ctx context.Context, node string, ncpu int, mem int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.caps == nil {
		s.caps = map[string]int{}
	}
	s.caps[node] = ncpu
	return nil
}
func (s *capStore) Since(context.Context, time.Time) ([]metrics.Sample, error)   { return nil, nil }
func (s *capStore) Latest(context.Context, time.Time) ([]metrics.Sample, error)  { return nil, nil }
func (s *capStore) Prune(_ context.Context, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes++
	return nil
}
func (s *capStore) ListCapacity(context.Context) ([]metrics.NodeCapacity, error) { return nil, nil }
func (s *capStore) PruneCapacity(context.Context, time.Time) error               { return nil }
func (s *capStore) PruneCapacityExcept(_ context.Context, keep []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keptExcept = append([]string(nil), keep...)
	return nil
}

func TestSamplerTickTagsNodesAndCapacity(t *testing.T) {
	local := fakeSrc{stats: []docker.ContainerStat{{Component: "krill"}}, cap: docker.NodeInfo{NCPU: 2}}
	workerOK := fakeSrc{stats: []docker.ContainerStat{{Component: "app"}}, cap: docker.NodeInfo{NCPU: 4}}
	workers := func(ctx context.Context) ([]metrics.Worker, error) {
		return []metrics.Worker{{Name: "w1", Connect: func(context.Context) (metrics.NodeStatsSource, func() error, error) {
			return workerOK, func() error { return nil }, nil
		}}}, nil
	}
	cs := metrics.NewClusterSource("cp", local, workers, time.Second)
	st := &capStore{}
	s := metrics.NewSampler(cs, st, time.Hour, time.Hour)

	s.TickForTest(context.Background())

	st.mu.Lock()
	defer st.mu.Unlock()
	want := map[string]bool{"cp/krill": false, "w1/app": false}
	for _, k := range st.ins {
		want[k] = true
	}
	if !want["cp/krill"] || !want["w1/app"] {
		t.Fatalf("expected node-tagged inserts, got %v", st.ins)
	}
	if st.caps["cp"] != 2 || st.caps["w1"] != 4 {
		t.Fatalf("expected per-node capacity, got %v", st.caps)
	}
	// Orphan-capacity prune keeps exactly the current nodes (so a renamed/removed
	// node's stale capacity row would be dropped).
	keep := map[string]bool{}
	for _, n := range st.keptExcept {
		keep[n] = true
	}
	if len(st.keptExcept) != 2 || !keep["cp"] || !keep["w1"] {
		t.Fatalf("expected PruneCapacityExcept keep={cp,w1}, got %v", st.keptExcept)
	}
}

// TestSamplerPruneCadence verifies that Prune fires on tick 1 and then exactly
// once more on tick pruneEvery (not on intermediate ticks).
func TestSamplerPruneCadence(t *testing.T) {
	noWorkers := func(ctx context.Context) ([]metrics.Worker, error) { return nil, nil }
	local := fakeSrc{stats: []docker.ContainerStat{{Component: "krill"}}, cap: docker.NodeInfo{NCPU: 1}}
	cs := metrics.NewClusterSource("cp", local, noWorkers, time.Second)
	st := &capStore{}
	s := metrics.NewSampler(cs, st, time.Hour, time.Hour)

	ctx := context.Background()
	// tick 1 — prune must fire
	s.TickForTest(ctx)
	st.mu.Lock()
	after1 := st.prunes
	st.mu.Unlock()
	if after1 != 1 {
		t.Fatalf("prune after tick 1: want 1, got %d", after1)
	}

	// ticks 2 .. pruneEvery-1 — prune must NOT fire
	for i := 2; i < metrics.PruneEvery; i++ {
		s.TickForTest(ctx)
	}
	st.mu.Lock()
	mid := st.prunes
	st.mu.Unlock()
	if mid != 1 {
		t.Fatalf("prune after ticks 2..%d-1: want still 1, got %d", metrics.PruneEvery, mid)
	}

	// tick pruneEvery — prune must fire again
	s.TickForTest(ctx)
	st.mu.Lock()
	final := st.prunes
	st.mu.Unlock()
	if final != 2 {
		t.Fatalf("prune after tick %d: want 2, got %d", metrics.PruneEvery, final)
	}
}

// TestSamplerSelfCompLearned verifies that the sampler learns the control-plane
// component name from the first container stat that has SelfControl=true.
func TestSamplerSelfCompLearned(t *testing.T) {
	noWorkers := func(ctx context.Context) ([]metrics.Worker, error) { return nil, nil }
	local := fakeSrc{
		stats: []docker.ContainerStat{{Component: "krill", SelfControl: true}},
		cap:   docker.NodeInfo{NCPU: 1},
	}
	cs := metrics.NewClusterSource("cp", local, noWorkers, time.Second)
	st := &capStore{}
	s := metrics.NewSampler(cs, st, time.Hour, time.Hour)

	s.TickForTest(context.Background())

	if got := s.SelfComponent(); got != "krill" {
		t.Fatalf("SelfComponent() = %q, want %q", got, "krill")
	}
}

// When the worker list itself cannot be read (a transient DB failure in the
// lister), SampleAll returned only the control-plane sample — indistinguishable
// from "this cluster has no workers". The tick then pruned every worker's
// capacity row, so the monitoring page lost NCPU/MemTotal for the whole cluster
// until each worker was sampled again.
func TestTickSkipsOrphanPruneWhenWorkerListUnavailable(t *testing.T) {
	local := fakeSrc{stats: []docker.ContainerStat{{Component: "krill"}}, cap: docker.NodeInfo{NCPU: 2}}
	workers := func(ctx context.Context) ([]metrics.Worker, error) {
		return nil, errors.New("pool exhausted")
	}
	cs := metrics.NewClusterSource("cp", local, workers, time.Second)
	st := &capStore{}
	s := metrics.NewSampler(cs, st, time.Hour, time.Hour)

	s.TickForTest(context.Background())

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.keptExcept != nil {
		t.Errorf("pruned orphan capacity to %v while the worker list was unavailable: every worker's capacity row would be dropped", st.keptExcept)
	}
	// The control-plane sample itself must still be recorded.
	if st.caps["cp"] != 2 {
		t.Errorf("control-plane capacity not recorded: %v", st.caps)
	}
}

// A NodeInfo failure left Capacity zero but still reported the node OK, so the
// tick upserted NCPU=0/MemTotal=0 over good data: the dashboard then showed a
// node with no capacity and a memory percentage divided by zero.
func TestNodeInfoFailureDoesNotZeroCapacity(t *testing.T) {
	local := fakeSrc{stats: []docker.ContainerStat{{Component: "krill"}}, capErr: errors.New("daemon busy")}
	workers := func(ctx context.Context) ([]metrics.Worker, error) { return nil, nil }
	cs := metrics.NewClusterSource("cp", local, workers, time.Second)
	st := &capStore{}
	s := metrics.NewSampler(cs, st, time.Hour, time.Hour)

	s.TickForTest(context.Background())

	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.caps["cp"]; ok {
		t.Errorf("capacity upserted as %v despite NodeInfo failing: good values are overwritten with zeros", st.caps)
	}
	// Container stats are still useful and must keep flowing.
	if len(st.ins) == 0 {
		t.Error("container stats dropped along with the capacity")
	}
}
