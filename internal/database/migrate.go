package database

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" and "pgx/v5" drivers for database/sql
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies all up-migrations. Idempotent (ErrNoChange = success).
//
// Self-update rolls the binary back to the previous release on a failed
// upgrade, so an older binary must be able to start against a schema a
// newer one already migrated forward: if the recorded version is ahead of
// what this binary embeds, RunMigrations logs a warning and returns without
// calling Up() rather than failing with "no migration found for version N".
// This is safe only because migrations are required to be additive relative
// to the previous release — see the compatibility policy in CLAUDE.md §4.
func RunMigrations(dsn string) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	sqlDB, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

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

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
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
