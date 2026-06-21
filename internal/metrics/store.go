package metrics

import (
	"context"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
)

// Sample is one stored metric row.
type Sample struct {
	Node          string
	Component     string
	TS            time.Time
	CPUPct        float64
	MemBytes      int64
	MemLimitBytes int64
}

// NodeCapacity holds the last-known hardware capacity for one cluster node.
type NodeCapacity struct {
	Node      string
	NCPU      int
	MemTotal  int64
	SampledAt time.Time
}

// Store is the persistence the metrics package needs.
type Store interface {
	Insert(ctx context.Context, node, component string, cpu float64, mem, memLimit int64) error
	Since(ctx context.Context, since time.Time) ([]Sample, error)
	// Latest returns the most recent sample per (node, component), considering
	// only samples at or after `since` so components that stopped reporting
	// (dead containers) drop out of the "current" snapshot.
	Latest(ctx context.Context, since time.Time) ([]Sample, error)
	Prune(ctx context.Context, before time.Time) error
	UpsertCapacity(ctx context.Context, node string, ncpu int, memTotal int64) error
	ListCapacity(ctx context.Context) ([]NodeCapacity, error)
	PruneCapacity(ctx context.Context, before time.Time) error
	// PruneCapacityExcept drops capacity rows for nodes not in keep, so a renamed
	// or removed node stops appearing (a still-listed but down node is kept, and
	// shows stale). No-op when keep is empty (guarded by the caller).
	PruneCapacityExcept(ctx context.Context, keep []string) error
}

// DBStore implements Store backed by the sqlc-generated queries.
type DBStore struct{ q *db.Queries }

// NewDBStore creates a DBStore using the provided Queries.
func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) Insert(ctx context.Context, node, component string, cpu float64, mem, memLimit int64) error {
	return s.q.InsertMetricSample(ctx, db.InsertMetricSampleParams{
		Node:          node,
		Component:     component,
		CpuPct:        float32(cpu),
		MemBytes:      mem,
		MemLimitBytes: memLimit,
	})
}

func (s *DBStore) Since(ctx context.Context, since time.Time) ([]Sample, error) {
	rows, err := s.q.MetricSamplesSince(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(rows))
	for _, r := range rows {
		out = append(out, Sample{
			Node:          r.Node,
			Component:     r.Component,
			TS:            r.Ts,
			CPUPct:        float64(r.CpuPct),
			MemBytes:      r.MemBytes,
			MemLimitBytes: r.MemLimitBytes,
		})
	}
	return out, nil
}

func (s *DBStore) Latest(ctx context.Context, since time.Time) ([]Sample, error) {
	rows, err := s.q.LatestMetricSamples(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(rows))
	for _, r := range rows {
		out = append(out, Sample{
			Node:          r.Node,
			Component:     r.Component,
			TS:            r.Ts,
			CPUPct:        float64(r.CpuPct),
			MemBytes:      r.MemBytes,
			MemLimitBytes: r.MemLimitBytes,
		})
	}
	return out, nil
}

func (s *DBStore) Prune(ctx context.Context, before time.Time) error {
	return s.q.PruneMetricSamples(ctx, before)
}

func (s *DBStore) UpsertCapacity(ctx context.Context, node string, ncpu int, memTotal int64) error {
	return s.q.UpsertNodeCapacity(ctx, db.UpsertNodeCapacityParams{
		Node:          node,
		Ncpu:          int32(ncpu),
		MemTotalBytes: memTotal,
	})
}

func (s *DBStore) ListCapacity(ctx context.Context) ([]NodeCapacity, error) {
	rows, err := s.q.ListNodeCapacity(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeCapacity, 0, len(rows))
	for _, r := range rows {
		out = append(out, NodeCapacity{
			Node:      r.Node,
			NCPU:      int(r.Ncpu),
			MemTotal:  r.MemTotalBytes,
			SampledAt: r.SampledAt,
		})
	}
	return out, nil
}

func (s *DBStore) PruneCapacity(ctx context.Context, before time.Time) error {
	return s.q.PruneNodeCapacity(ctx, before)
}

func (s *DBStore) PruneCapacityExcept(ctx context.Context, keep []string) error {
	return s.q.PruneNodeCapacityExcept(ctx, keep)
}
