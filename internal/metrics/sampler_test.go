package metrics_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
)

type capStore struct {
	mu   sync.Mutex
	ins  []string // "node/comp"
	caps map[string]int
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
func (s *capStore) Since(context.Context, time.Time) ([]metrics.Sample, error)    { return nil, nil }
func (s *capStore) Latest(context.Context, time.Time) ([]metrics.Sample, error)   { return nil, nil }
func (s *capStore) Prune(context.Context, time.Time) error                        { return nil }
func (s *capStore) ListCapacity(context.Context) ([]metrics.NodeCapacity, error)  { return nil, nil }
func (s *capStore) PruneCapacity(context.Context, time.Time) error                { return nil }

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
}
