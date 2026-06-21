package metrics_test

import (
	"context"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/testutil"
)

func TestStoreInsertLatestPrune(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := metrics.NewDBStore(db.New(pool))
	ctx := context.Background()

	if err := st.Insert(ctx, "node-1", "krill-7", 12.5, 200<<20, 512<<20); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Insert(ctx, "node-1", "krill-7", 18.0, 220<<20, 512<<20); err != nil {
		t.Fatalf("insert2: %v", err)
	}
	latest, err := st.Latest(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(latest) != 1 {
		t.Fatalf("latest: %v n=%d", err, len(latest))
	}
	if latest[0].CPUPct < 17 || latest[0].CPUPct > 19 {
		t.Fatalf("latest cpu = %v, want ~18", latest[0].CPUPct)
	}
	// A cutoff in the future excludes the (older) samples, so nothing is "current".
	if stale, _ := st.Latest(ctx, time.Now().Add(time.Hour)); len(stale) != 0 {
		t.Fatalf("latest with future cutoff: want 0, got %d", len(stale))
	}
	if since, _ := st.Since(ctx, time.Now().Add(-time.Hour)); len(since) != 2 {
		t.Fatalf("since: want 2, got %d", len(since))
	}
	if err := st.Prune(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if since, _ := st.Since(ctx, time.Now().Add(-time.Hour)); len(since) != 0 {
		t.Fatalf("after prune want 0, got %d", len(since))
	}
}

func TestStoreNodeDimension(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := metrics.NewDBStore(db.New(pool))
	ctx := context.Background()

	// same component name on two different nodes must NOT collapse in Latest
	if err := st.Insert(ctx, "node-a", "krill-1", 10, 100, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Insert(ctx, "node-b", "krill-1", 20, 200, 0); err != nil {
		t.Fatal(err)
	}
	latest, err := st.Latest(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("want 2 latest (one per node), got %d", len(latest))
	}
	nodes := map[string]bool{}
	for _, s := range latest {
		nodes[s.Node] = true
	}
	if !nodes["node-a"] || !nodes["node-b"] {
		t.Fatalf("expected both nodes, got %v", nodes)
	}
}

func TestStoreCapacityUpsert(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := metrics.NewDBStore(db.New(pool))
	ctx := context.Background()

	if err := st.UpsertCapacity(ctx, "node-a", 4, 8<<30); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCapacity(ctx, "node-a", 8, 16<<30); err != nil { // overwrite
		t.Fatal(err)
	}
	caps, err := st.ListCapacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 1 || caps[0].NCPU != 8 || caps[0].MemTotal != 16<<30 {
		t.Fatalf("upsert did not overwrite: %+v", caps)
	}
}

func TestStoreCapacityPruneExcept(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := metrics.NewDBStore(db.New(pool))
	ctx := context.Background()

	for _, n := range []string{"control-plane", "worker-1", "old-node"} {
		if err := st.UpsertCapacity(ctx, n, 1, 1<<30); err != nil {
			t.Fatal(err)
		}
	}
	// Keep only the current cluster; "old-node" (renamed/removed) must be dropped.
	if err := st.PruneCapacityExcept(ctx, []string{"control-plane", "worker-1"}); err != nil {
		t.Fatal(err)
	}
	caps, err := st.ListCapacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range caps {
		got[c.Node] = true
	}
	if len(caps) != 2 || !got["control-plane"] || !got["worker-1"] || got["old-node"] {
		t.Fatalf("PruneCapacityExcept wrong result: %+v", caps)
	}
}
