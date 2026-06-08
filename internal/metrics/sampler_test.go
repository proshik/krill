package metrics

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
)

type fakeSrc struct{ stats []docker.ContainerStat }

func (f *fakeSrc) ListContainerStats(context.Context) ([]docker.ContainerStat, error) {
	return f.stats, nil
}

type recStore struct {
	mu       sync.Mutex
	inserts  []string
	prunedTo []time.Time
}

func (r *recStore) Insert(_ context.Context, c string, _ float64, _, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inserts = append(r.inserts, c)
	return nil
}
func (r *recStore) Since(context.Context, time.Time) ([]Sample, error) { return nil, nil }
func (r *recStore) Latest(context.Context) ([]Sample, error)           { return nil, nil }
func (r *recStore) Prune(_ context.Context, before time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prunedTo = append(r.prunedTo, before)
	return nil
}

func TestSamplerTickInsertsAndPrunes(t *testing.T) {
	src := &fakeSrc{stats: []docker.ContainerStat{
		{Component: "krill-7", CPUPct: 5},
		{Component: "traefik", CPUPct: 1},
	}}
	st := &recStore{}
	s := NewSampler(src, st, time.Minute, time.Hour)
	s.tick(context.Background())
	if len(st.inserts) != 2 {
		t.Fatalf("want 2 inserts, got %v", st.inserts)
	}
	if len(st.prunedTo) != 1 {
		t.Fatalf("tick should prune once, got %d", len(st.prunedTo))
	}
}
