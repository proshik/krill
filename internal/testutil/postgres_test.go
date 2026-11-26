package testutil

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewTestDBIsolatedMigratedDatabases(t *testing.T) {
	ctx := context.Background()
	var names [2]string
	t.Run("pair", func(t *testing.T) {
		a, b := NewTestDB(t), NewTestDB(t)
		if err := a.QueryRow(ctx, "SELECT current_database()").Scan(&names[0]); err != nil {
			t.Fatal(err)
		}
		if err := b.QueryRow(ctx, "SELECT current_database()").Scan(&names[1]); err != nil {
			t.Fatal(err)
		}
		if names[0] == names[1] {
			t.Fatalf("both calls share database %q", names[0])
		}
		if !strings.Contains(names[0], "pair") {
			t.Errorf("database name %q does not name the test", names[0])
		}

		// Both are fully migrated: the recorded version is the newest
		// migration, and clean.
		latest := latestMigration(t)
		for _, pool := range []*pgxpool.Pool{a, b} {
			var version int64
			var dirty bool
			if err := pool.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
				t.Fatalf("schema_migrations: %v", err)
			}
			if version != latest || dirty {
				t.Fatalf("not migrated: version=%d dirty=%v, want %d clean", version, dirty, latest)
			}
		}

		// A row written in one database is invisible in the other.
		if _, err := a.Exec(ctx, "INSERT INTO users (email, password_hash) VALUES ('isolated@example.test', 'unused')"); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := b.QueryRow(ctx, "SELECT count(*) FROM users WHERE email = 'isolated@example.test'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("the second database sees %d rows written to the first", n)
		}

		// The pool's DSN names its own database, for callers that derive one.
		if got := a.Config().ConnConfig.Database; got != names[0] {
			t.Fatalf("pool config database = %q, want %q", got, names[0])
		}
	})

	// The subtest's cleanup dropped both databases.
	for _, name := range names {
		var exists bool
		if err := shared.admin.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("database %q survived its test", name)
		}
	}
}

func TestTestDBName(t *testing.T) {
	got := testDBName("TestSomething/with spaces-and.dots", 7)
	if got != "t7_testsomething_with_spaces_and_dots" {
		t.Fatalf("testDBName = %q", got)
	}
	long := testDBName(strings.Repeat("Ä", 100)+strings.Repeat("x", 100), 123456)
	if len(long) > 63 {
		t.Fatalf("name is %d bytes, over Postgres' 63-byte limit: %q", len(long), long)
	}
}

// latestMigration is the highest version under internal/database/migrations.
func latestMigration(t *testing.T) int64 {
	t.Helper()
	entries, err := os.ReadDir("../database/migrations")
	if err != nil {
		t.Fatal(err)
	}
	var latest int64
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(prefix, 10, 64); err == nil && v > latest {
			latest = v
		}
	}
	if latest == 0 {
		t.Fatal("no migrations found")
	}
	return latest
}
