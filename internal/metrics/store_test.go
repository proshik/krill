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

	if err := st.Insert(ctx, "krill-7", 12.5, 200<<20, 512<<20); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Insert(ctx, "krill-7", 18.0, 220<<20, 512<<20); err != nil {
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
