package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// pruneEvery runs the retention DELETE once per this many ticks (rather than
// every tick) so each prune removes a meaningful chunk in one pass instead of
// generating constant small deletes + autovacuum churn on the tiny VPS.
const pruneEvery = 120 // ~1h at the default 30s interval

// diskEvery takes a disk snapshot once per this many ticks: disks fill over
// hours, and on a worker every snapshot is one more SSH command.
const diskEvery = 10 // ~5min at the default 30s interval

// Tick is the result of one sampling pass, handed to the Observer.
type Tick struct {
	Start    time.Time
	Nodes    []NodeSample
	Complete bool // the node list covers the whole cluster (see SampleAll)
	Disks    bool // this tick asked every node for a disk snapshot
}

// Observer is told about every finished tick (e.g. the notification
// resource monitor). It runs on the sampler goroutine, so it must not block
// on slow work.
type Observer interface {
	Observe(ctx context.Context, t Tick)
}

// Sampler periodically records container stats and prunes old history.
type Sampler struct {
	src       *ClusterSource
	store     Store
	interval  time.Duration
	retention time.Duration
	log       *slog.Logger

	ticks int // tick counter for the coarse prune and disk cadences
	obs   Observer

	mu       sync.Mutex
	selfComp string // control-plane component, learned from ListContainerStats
}

// NewSampler creates a Sampler. Zero/negative interval defaults to 30s;
// zero/negative retention defaults to 48h.
func NewSampler(src *ClusterSource, store Store, interval, retention time.Duration) *Sampler {
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

// SetObserver registers the tick observer. Call before Run.
func (s *Sampler) SetObserver(o Observer) { s.obs = o }

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
	now := time.Now()
	withDisks := s.ticks%diskEvery == 0
	samples, complete := s.src.Sample(ctx, withDisks)
	keep := make([]string, 0, len(samples))
	for _, ns := range samples {
		keep = append(keep, ns.Node) // every current node (incl. down → kept, shows stale)
		if !ns.OK {
			continue // unreachable node already logged; leave its data stale
		}
		for _, st := range ns.Containers {
			if st.SelfControl {
				s.mu.Lock()
				s.selfComp = st.Component
				s.mu.Unlock()
			}
			if err := s.store.Insert(ctx, ns.Node, st.Component, st.CPUPct, st.MemBytes, st.MemLimitBytes); err != nil {
				s.log.Warn("metrics: insert failed", "node", ns.Node, "component", st.Component, "err", err)
			}
		}
		if ns.CapacityOK {
			if err := s.store.UpsertCapacity(ctx, ns.Node, ns.Capacity.NCPU, ns.Capacity.MemTotal); err != nil {
				s.log.Warn("metrics: capacity upsert failed", "node", ns.Node, "err", err)
			}
		}
		if ns.DisksOK {
			if err := s.store.ReplaceDisks(ctx, ns.Node, ns.Disks); err != nil {
				s.log.Warn("metrics: disk snapshot store failed", "node", ns.Node, "err", err)
			}
		}
	}
	// Drop capacity for nodes no longer in the cluster (renamed/removed) so they
	// stop showing as stale phantoms; down-but-still-listed nodes stay in keep.
	// Only safe when the node list is complete: an unreadable worker list would
	// otherwise look like "every worker was removed" and wipe their capacity.
	if complete && len(keep) > 0 {
		if err := s.store.PruneCapacityExcept(ctx, keep); err != nil {
			s.log.Warn("metrics: orphan capacity prune failed", "err", err)
		}
		if err := s.store.PruneDisksExcept(ctx, keep); err != nil {
			s.log.Warn("metrics: orphan disk prune failed", "err", err)
		}
	}
	s.ticks++
	if s.ticks == 1 || s.ticks%pruneEvery == 0 {
		before := now.Add(-s.retention)
		if err := s.store.Prune(ctx, before); err != nil {
			s.log.Warn("metrics: prune failed", "err", err)
		}
		if err := s.store.PruneCapacity(ctx, before); err != nil {
			s.log.Warn("metrics: capacity prune failed", "err", err)
		}
		if err := s.store.PruneDisks(ctx, before); err != nil {
			s.log.Warn("metrics: disk prune failed", "err", err)
		}
	}
	if s.obs != nil {
		s.obs.Observe(ctx, Tick{Start: now, Nodes: samples, Complete: complete, Disks: withDisks})
	}
}
