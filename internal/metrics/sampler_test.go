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
func (r *recStore) Since(context.Context, time.Time) ([]Sample, error)  { return nil, nil }
func (r *recStore) Latest(context.Context, time.Time) ([]Sample, error) { return nil, nil }
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
	// The first tick prunes (startup), but subsequent ticks must NOT prune every
	// time — only once per pruneEvery — to avoid constant small deletes.
	if len(st.prunedTo) != 1 {
		t.Fatalf("first tick should prune once, got %d", len(st.prunedTo))
	}
	for i := 0; i < pruneEvery-2; i++ { // ticks 2..(pruneEvery-1)
		s.tick(context.Background())
	}
	if len(st.prunedTo) != 1 {
		t.Fatalf("ticks 2..%d must not prune, got %d prunes", pruneEvery-1, len(st.prunedTo))
	}
	s.tick(context.Background()) // tick == pruneEvery → prune again
	if len(st.prunedTo) != 2 {
		t.Fatalf("tick %d should prune, got %d prunes", pruneEvery, len(st.prunedTo))
	}
}

// TestSamplerLearnsSelfComponent verifies the sampler records the control-plane
// component from ListContainerStats (so the handler need not scan docker).
func TestSamplerLearnsSelfComponent(t *testing.T) {
	src := &fakeSrc{stats: []docker.ContainerStat{
		{Component: "krill-self", SelfControl: true},
		{Component: "traefik"},
	}}
	s := NewSampler(src, &recStore{}, time.Minute, time.Hour)
	if s.SelfComponent() != "" {
		t.Fatalf("self component should be empty before the first tick")
	}
	s.tick(context.Background())
	if got := s.SelfComponent(); got != "krill-self" {
		t.Fatalf("self component = %q, want krill-self", got)
	}
}
