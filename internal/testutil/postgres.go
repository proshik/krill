package testutil

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/proshik/krill/internal/database"
)

// templateDB holds the migrated schema every test database is cloned from.
// It accepts no connections: CREATE DATABASE ... TEMPLATE refuses a source
// database that has other sessions.
const templateDB = "krill_template"

// sharedPG is the one Postgres server of a test binary. It is started on the
// first NewTestDB call and lives as long as the process: no test owns it, and
// testcontainers' reaper (Ryuk) removes the container after the binary exits.
type sharedPG struct {
	baseDSN string        // superuser DSN; the database name is replaced per test
	admin   *pgxpool.Pool // connected to the "postgres" database, for CREATE/DROP DATABASE
}

var (
	sharedOnce sync.Once
	shared     *sharedPG
	sharedErr  error
	dbCounter  atomic.Int64
)

// NewTestDB returns a pool on a fresh, fully migrated database of its own.
// The database is cloned from a template migrated once per test binary on a
// shared Postgres container, and is dropped when the test finishes. Roles are
// server-wide and outlive the database, so a test must not create one with a
// fixed name here. Tests that need a server of their own (restarting it,
// changing server settings, or running migrations against an empty server)
// start their own container.
func NewTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	sharedOnce.Do(func() { shared, sharedErr = startSharedPG(context.Background()) })
	if sharedErr != nil {
		t.Fatalf("shared postgres: %v", sharedErr)
	}
	ctx := context.Background()

	name := testDBName(t.Name(), dbCounter.Add(1))
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := shared.admin.Exec(ctx, "CREATE DATABASE "+ident+" TEMPLATE "+templateDB); err != nil {
		t.Fatalf("create test database %s: %v", name, err)
	}
	// Registered first so it runs last, after the pool below is closed. FORCE
	// also ends any session a test opened on its own and left behind.
	t.Cleanup(func() {
		if _, err := shared.admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})

	dsn, err := withDatabase(shared.baseDSN, name)
	if err != nil {
		t.Fatalf("test database dsn: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// startSharedPG starts the container, migrates the template database and
// closes it to connections.
func startSharedPG(ctx context.Context) (*sharedPG, error) {
	pg, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("krill"),
		postgres.WithUsername("krill"),
		postgres.WithPassword("krill"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if pg != nil {
			_ = pg.Terminate(ctx)
		}
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, fmt.Errorf("connection string: %w", err)
	}
	adminDSN, err := withDatabase(dsn, "postgres")
	if err != nil {
		return nil, err
	}
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return nil, fmt.Errorf("admin pool: %w", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDB); err != nil {
		admin.Close()
		return nil, fmt.Errorf("create template database: %w", err)
	}
	templateDSN, err := withDatabase(dsn, templateDB)
	if err != nil {
		admin.Close()
		return nil, err
	}
	if err := database.RunMigrations(templateDSN); err != nil {
		admin.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	// RunMigrations closed its connections; terminate any straggler anyway so
	// the first CREATE DATABASE ... TEMPLATE does not find a session on it.
	for _, sql := range []string{
		"ALTER DATABASE " + templateDB + " WITH IS_TEMPLATE true ALLOW_CONNECTIONS false",
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '" + templateDB + "'",
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			admin.Close()
			return nil, fmt.Errorf("seal template database: %w", err)
		}
	}
	return &sharedPG{baseDSN: dsn, admin: admin}, nil
}

// withDatabase returns dsn pointing at database name.
func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// testDBName builds a database name unique within the test binary that still
// says which test made it: "t<n>_" plus the test name in lowercase letters,
// digits and underscores, cut to Postgres' 63-byte identifier limit.
func testDBName(testName string, n int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "t%d_", n)
	for _, r := range strings.ToLower(testName) {
		if b.Len() >= 63 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
