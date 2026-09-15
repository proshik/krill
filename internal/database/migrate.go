package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" and "pgx/v5" drivers for database/sql
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies all up-migrations with no way to stop them early;
// see RunMigrationsContext.
func RunMigrations(dsn string) error {
	return RunMigrationsContext(context.Background(), dsn)
}

// RunMigrationsContext applies all up-migrations. Idempotent (ErrNoChange =
// success).
//
// Self-update rolls the binary back to the previous release on a failed
// upgrade, so an older binary must be able to start against a schema a
// newer one already migrated forward: if the recorded version is ahead of
// what this binary embeds, it logs a warning and returns without calling
// Up() rather than failing with "no migration found for version N". This is
// safe only because migrations are required to be additive relative to the
// previous release — see the compatibility policy in CLAUDE.md §4.
//
// Cancelling ctx (a shutdown signal) never interrupts a migration halfway:
// golang-migrate marks the schema dirty before each migration and clean
// after it, and a dirty schema stops every binary from starting — the
// previous release a rollback puts back included. The in-flight migration
// finishes, no further one starts, and the call returns an error wrapping
// ctx.Err() even when nothing was left to apply, so the caller does not go
// on starting as if the schema were current.
func RunMigrationsContext(ctx context.Context, dsn string) error {
	if err := runMigrations(ctx, dsn); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("migrations stopped by shutdown: %w", err)
	}
	return nil
}

func runMigrations(ctx context.Context, dsn string) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	sqlDB, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	// golang-migrate pings and takes its advisory lock with
	// context.Background(), so a shutdown while the database does not answer
	// would otherwise wait for systemd's stop timeout. Pinging with ctx first
	// makes that wait end with the signal.
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	driver, err := postgres.WithInstance(sqlDB, &postgres.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return err
	}

	v, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return err
	}
	if err == nil {
		if dirty {
			return fmt.Errorf("database schema is dirty at version %d; fix it manually before starting", v)
		}
		highest, err := maxEmbeddedVersion()
		if err != nil {
			return err
		}
		if v > highest {
			slog.Warn("database schema is newer than this binary; running without migrating", "schema", v, "binary", highest)
			return nil
		}
	}

	// GracefulStop lets the running migration finish and makes Up return
	// before the next one. The goroutine exits as soon as Up returns.
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			select {
			case m.GracefulStop <- true:
			default: // a stop is already pending
			}
		case <-done:
		}
	}()
	err = m.Up()
	close(done)
	<-exited
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// maxEmbeddedVersion returns the highest migration version embedded in this
// binary, parsed from the leading decimal digits of each *.up.sql file name
// under migrations/ (e.g. "000046_panel_hsts.up.sql" -> 46).
func maxEmbeddedVersion() (uint, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, err
	}

	var highest uint
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		v, ok := migrationVersion(e.Name())
		if !ok {
			continue
		}
		if !found || v > highest {
			highest = v
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("no migration files found under migrations/")
	}
	return highest, nil
}

// migrationVersion parses the leading decimal digits of a migration file
// name (e.g. "000047_add_widgets.up.sql" -> 47, true).
func migrationVersion(name string) (uint, bool) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	v, err := strconv.ParseUint(name[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	return uint(v), true
}
