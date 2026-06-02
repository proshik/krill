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
		 WHERE table_schema='public'
		 AND table_name IN ('users','sessions','organizations','members','projects','environments','applications','deployments','postgres_dbs','redis_dbs')`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 10 {
		t.Fatalf("expected 10 tables, got %d", n)
	}
}
