package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// pruneEvery runs the retention DELETE once per this many ticks (rather than
// every tick) so each prune removes a meaningful chunk in one pass instead of
// generating constant small deletes + autovacuum churn on the tiny VPS.
const pruneEvery = 120 // ~1h at the default 30s interval

// statsSource is the subset of docker.Engine the sampler needs.
type statsSource interface {
	ListContainerStats(ctx context.Context) ([]docker.ContainerStat, error)
}

// Sampler periodically records container stats and prunes old history.
type Sampler struct {
	src       statsSource
	store     Store
	interval  time.Duration
	retention time.Duration
	log       *slog.Logger

	ticks int // tick counter for the coarse prune cadence

	mu       sync.Mutex
	selfComp string // control-plane component, learned from ListContainerStats
}

// NewSampler creates a Sampler. Zero/negative interval defaults to 30s;
// zero/negative retention defaults to 48h.
func NewSampler(src statsSource, store Store, interval, retention time.Duration) *Sampler {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if retention <= 0 {
		retention = 48 * time.Hour
	}
	return &Sampler{
		src:       src,
		store:     store,
		interval:  interval,
		retention: retention,
		log:       slog.Default(),
	}
}

// Run samples on a ticker until ctx is canceled.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// SelfComponent returns the component key of Krill's own container, learned as
// a byproduct of sampling (empty until the first tick, or when Krill runs
// outside a container). Lets the monitoring handler classify the control plane
// without its own live docker scan.
func (s *Sampler) SelfComponent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selfComp
}

func (s *Sampler) tick(ctx context.Context) {
	stats, err := s.src.ListContainerStats(ctx)
	if err != nil {
		s.log.Warn("metrics: list stats failed", "err", err)
		return
	}
	for _, st := range stats {
		if st.SelfControl {
			s.mu.Lock()
			s.selfComp = st.Component
			s.mu.Unlock()
		}
		if err := s.store.Insert(ctx, st.Component, st.CPUPct, st.MemBytes, st.MemLimitBytes); err != nil {
			s.log.Warn("metrics: insert failed", "component", st.Component, "err", err)
		}
	}
	// Prune once at startup (clear any backlog) and then only every pruneEvery
	// ticks, not on every tick.
	s.ticks++
	if s.ticks == 1 || s.ticks%pruneEvery == 0 {
		if err := s.store.Prune(ctx, time.Now().Add(-s.retention)); err != nil {
			s.log.Warn("metrics: prune failed", "err", err)
		}
	}
}
