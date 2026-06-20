package database_test

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/testutil"
)

func TestMigrationsApply(t *testing.T) {
	pool := testutil.NewTestDB(t)

	// Expected set covers the full current schema (migrations 000001..000025;
	// 000025 adds app_ports).
	expectedTables := []string{
		"users", "sessions", "organizations", "members", "projects",
		"environments", "applications", "deployments", "postgres_dbs",
		"redis_dbs", "domains", "destinations", "backups", "registries",
		"notification_channels", "metric_samples", "app_volumes", "volume_backups",
		"app_db_links", "app_ports",
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

	// Migration 000026: auto_deploy and webhook_secret columns.
	for _, col := range []string{"auto_deploy", "webhook_secret"} {
		var exists bool
		err = pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_schema='public' AND table_name='applications' AND column_name=$1)`, col,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("query column %s: %v", col, err)
		}
		if !exists {
			t.Fatalf("applications.%s missing after migration", col)
		}
	}
}
