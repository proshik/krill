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
	// ReplaceDisks stores a node's disk snapshot, dropping its mounts the
	// snapshot no longer reports.
	ReplaceDisks(ctx context.Context, node string, disks []DiskUsage) error
	ListDisks(ctx context.Context) ([]NodeDisk, error)
	PruneDisks(ctx context.Context, before time.Time) error
	// PruneDisksExcept drops disks of nodes not in keep (same contract as
	// PruneCapacityExcept).
	PruneDisksExcept(ctx context.Context, keep []string) error
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

// SinceFor is Since limited to the given components: a reader that needs
// only some services' samples (an organization's apps) need not load the
// whole cluster's day.
func (s *DBStore) SinceFor(ctx context.Context, since time.Time, components []string) ([]Sample, error) {
	if len(components) == 0 {
		return nil, nil
	}
	rows, err := s.q.MetricSamplesSinceForComponents(ctx, db.MetricSamplesSinceForComponentsParams{Ts: since, Components: components})
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

func (s *DBStore) ReplaceDisks(ctx context.Context, node string, disks []DiskUsage) error {
	mounts := make([]string, 0, len(disks))
	for _, d := range disks {
		if err := s.q.UpsertNodeDisk(ctx, db.UpsertNodeDiskParams{
			Node: node, Mount: d.Mount, Filesystem: d.Filesystem,
			TotalBytes: d.Total, UsedBytes: d.Used, AvailBytes: d.Avail,
		}); err != nil {
			return err
		}
		mounts = append(mounts, d.Mount)
	}
	return s.q.PruneNodeDiskMounts(ctx, db.PruneNodeDiskMountsParams{Node: node, Mounts: mounts})
}

func (s *DBStore) ListDisks(ctx context.Context) ([]NodeDisk, error) {
	rows, err := s.q.ListNodeDisks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeDisk, 0, len(rows))
	for _, r := range rows {
		out = append(out, NodeDisk{
			Node: r.Node,
			DiskUsage: DiskUsage{Mount: r.Mount, Filesystem: r.Filesystem,
				Total: r.TotalBytes, Used: r.UsedBytes, Avail: r.AvailBytes},
			SampledAt: r.SampledAt,
		})
	}
	return out, nil
}

func (s *DBStore) PruneDisks(ctx context.Context, before time.Time) error {
	return s.q.PruneNodeDisks(ctx, before)
}

func (s *DBStore) PruneDisksExcept(ctx context.Context, keep []string) error {
	return s.q.PruneNodeDisksExcept(ctx, keep)
}
