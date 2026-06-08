package metrics

import (
	"context"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
)

// Sample is one stored metric row.
type Sample struct {
	Component     string
	TS            time.Time
	CPUPct        float64
	MemBytes      int64
	MemLimitBytes int64
}

// Store is the persistence the metrics package needs.
type Store interface {
	Insert(ctx context.Context, component string, cpu float64, mem, memLimit int64) error
	Since(ctx context.Context, since time.Time) ([]Sample, error)
	Latest(ctx context.Context) ([]Sample, error)
	Prune(ctx context.Context, before time.Time) error
}

// DBStore implements Store backed by the sqlc-generated queries.
type DBStore struct{ q *db.Queries }

// NewDBStore creates a DBStore using the provided Queries.
func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) Insert(ctx context.Context, component string, cpu float64, mem, memLimit int64) error {
	return s.q.InsertMetricSample(ctx, db.InsertMetricSampleParams{
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
			Component:     r.Component,
			TS:            r.Ts,
			CPUPct:        float64(r.CpuPct),
			MemBytes:      r.MemBytes,
			MemLimitBytes: r.MemLimitBytes,
		})
	}
	return out, nil
}

func (s *DBStore) Latest(ctx context.Context) ([]Sample, error) {
	rows, err := s.q.LatestMetricSamples(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(rows))
	for _, r := range rows {
		out = append(out, Sample{
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
