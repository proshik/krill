package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/proshik/krill/internal/docker"
)

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

func (s *Sampler) tick(ctx context.Context) {
	stats, err := s.src.ListContainerStats(ctx)
	if err != nil {
		s.log.Warn("metrics: list stats failed", "err", err)
		return
	}
	for _, st := range stats {
		if err := s.store.Insert(ctx, st.Component, st.CPUPct, st.MemBytes, st.MemLimitBytes); err != nil {
			s.log.Warn("metrics: insert failed", "component", st.Component, "err", err)
		}
	}
	if err := s.store.Prune(ctx, time.Now().Add(-s.retention)); err != nil {
		s.log.Warn("metrics: prune failed", "err", err)
	}
}
