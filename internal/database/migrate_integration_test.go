package database_test

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/testutil"
)

func TestMigrationsApply(t *testing.T) {
	pool := testutil.NewTestDB(t)

	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public' AND table_name IN ('users','sessions','applications')`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 tables, got %d", n)
	}
}
