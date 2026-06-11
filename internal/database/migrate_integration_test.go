package database_test

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/testutil"
)

func TestMigrationsApply(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Expected set covers the full current schema (migrations 000001..000017;
	// 000017 adds FK indexes only, no new tables).
	expectedTables := []string{
		"users", "sessions", "organizations", "members", "projects",
		"environments", "applications", "deployments", "postgres_dbs",
		"redis_dbs", "domains", "destinations", "backups", "registries",
		"notification_channels", "metric_samples",
	}

	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema='public'
		 AND table_name = ANY($1)`,
		expectedTables,
	).Scan(&n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != len(expectedTables) {
		t.Fatalf("expected %d tables, got %d", len(expectedTables), n)
	}

	// Spot-check a couple of columns added in later migrations.
	var c int
	err = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='public'
		 AND (table_name='applications' AND column_name='registry_id')`,
	).Scan(&c)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	if c != 1 {
		t.Fatalf("expected applications.registry_id column, got %d", c)
	}
}
