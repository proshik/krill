package database

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startTestPostgres starts a throwaway Postgres container and returns its DSN.
func startTestPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("krill"), postgres.WithUsername("krill"), postgres.WithPassword("krill"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	testcontainers.CleanupContainer(t, pg)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	return dsn
}

// TestMaxEmbeddedVersion derives the expected highest version independently
// (by globbing and parsing the embedded *.up.sql file names itself, rather
// than reusing production parsing logic) so it doesn't just test
// maxEmbeddedVersion against itself, and floors it at 46 so the test doesn't
// silently stop meaning anything as new migrations are added.
func TestMaxEmbeddedVersion(t *testing.T) {
	names, err := fs.Glob(migrationsFS, "migrations/*.up.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no *.up.sql files found under migrations/")
	}
	var want uint
	for _, name := range names {
		base := path.Base(name)
		i := 0
		for i < len(base) && base[i] >= '0' && base[i] <= '9' {
			i++
		}
		if i == 0 {
			t.Fatalf("migration file name has no leading version: %s", base)
		}
		v, err := strconv.ParseUint(base[:i], 10, 64)
		if err != nil {
			t.Fatalf("parse version from %s: %v", base, err)
		}
		if uint(v) > want {
			want = uint(v)
		}
	}

	got, err := maxEmbeddedVersion()
	if err != nil {
		t.Fatalf("maxEmbeddedVersion: %v", err)
	}
	if got != want {
		t.Fatalf("maxEmbeddedVersion() = %d, want %d (derived independently from the embedded file names)", got, want)
	}
	if got < 46 {
		t.Fatalf("maxEmbeddedVersion() = %d, want >= 46", got)
	}
}

func TestRunMigrationsOnEmptyDatabase(t *testing.T) {
	dsn := startTestPostgres(t)

	if err := RunMigrations(dsn); err != nil {
		t.Fatalf("RunMigrations on empty database: %v", err)
	}

	m := newMigrator(t, dsn)
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if dirty {
		t.Fatalf("schema left dirty after RunMigrations")
	}
	max, err := maxEmbeddedVersion()
	if err != nil {
		t.Fatalf("maxEmbeddedVersion: %v", err)
	}
	if v != max {
		t.Fatalf("version after RunMigrations = %d, want %d", v, max)
	}
}

// A shutdown signal while migrations run must not leave the schema dirty: a
// dirty schema stops every binary from starting, the reverted one included.
// A context cancelled before the call exercises the same GracefulStop path a
// signal arriving mid-way takes — Up stops before the next migration — and the
// error says the run was cut short, so startup does not go on as if the schema
// were current.
func TestRunMigrationsContextStopsOnCancel(t *testing.T) {
	dsn := startTestPostgres(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunMigrationsContext(ctx, dsn)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunMigrationsContext with a cancelled context = %v, want an error wrapping context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "stopped by shutdown") {
		t.Errorf("error = %q, want it to say the migrations were stopped by shutdown", err)
	}

	m := newMigrator(t, dsn)
	v, dirty, verr := m.Version()
	if verr != nil && !errors.Is(verr, migrate.ErrNilVersion) {
		t.Fatalf("Version: %v", verr)
	}
	if dirty {
		t.Fatalf("schema left dirty at version %d after a cancelled run", v)
	}

	// The next start picks up where the stopped one left off.
	if err := RunMigrationsContext(context.Background(), dsn); err != nil {
		t.Fatalf("RunMigrationsContext after a cancelled run: %v", err)
	}
}

func TestRunMigrationsContextCompletes(t *testing.T) {
	dsn := startTestPostgres(t)

	if err := RunMigrationsContext(context.Background(), dsn); err != nil {
		t.Fatalf("RunMigrationsContext: %v", err)
	}

	m := newMigrator(t, dsn)
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	max, err := maxEmbeddedVersion()
	if err != nil {
		t.Fatalf("maxEmbeddedVersion: %v", err)
	}
	if dirty || v != max {
		t.Fatalf("after RunMigrationsContext: version %d dirty=%v, want %d clean", v, dirty, max)
	}
}

func TestRunMigrationsToleratesNewerSchema(t *testing.T) {
	dsn := startTestPostgres(t)

	// Bring the schema up to the binary's own max version first.
	if err := RunMigrations(dsn); err != nil {
		t.Fatalf("initial RunMigrations: %v", err)
	}

	max, err := maxEmbeddedVersion()
	if err != nil {
		t.Fatalf("maxEmbeddedVersion: %v", err)
	}

	// Simulate a rollback scenario: the database was migrated forward by a
	// newer binary (Force writes schema_migrations directly, bypassing the
	// migration source, which is exactly what a newer release's migrations
	// would have done).
	newer := int(max) + 1
	forcer := newMigrator(t, dsn)
	if err := forcer.Force(newer); err != nil {
		t.Fatalf("Force(%d): %v", newer, err)
	}

	if err := RunMigrations(dsn); err != nil {
		t.Fatalf("RunMigrations on newer schema: %v", err)
	}

	// The version must not be rewound by the older binary.
	m := newMigrator(t, dsn)
	v, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if dirty {
		t.Fatalf("schema left dirty after RunMigrations")
	}
	if v != uint(newer) {
		t.Fatalf("version after RunMigrations = %d, want %d (unchanged)", v, newer)
	}
}

func TestRunMigrationsRefusesDirtySchema(t *testing.T) {
	dsn := startTestPostgres(t)

	if err := RunMigrations(dsn); err != nil {
		t.Fatalf("initial RunMigrations: %v", err)
	}

	// Mark the schema dirty directly, bypassing migrate's own APIs (mirrors
	// what a crashed migration leaves behind).
	sqlDB, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(`UPDATE schema_migrations SET dirty = true`); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}

	err = RunMigrations(dsn)
	if err == nil {
		t.Fatalf("RunMigrations on dirty schema: want error, got nil")
	}
	if !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("RunMigrations error = %q, want it to mention \"dirty\"", err.Error())
	}
}
