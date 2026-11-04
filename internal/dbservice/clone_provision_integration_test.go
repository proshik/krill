//go:build integration

package dbservice

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Runs the actual psql scripts, including transaction boundaries and \gexec,
// on one disposable PG server. No Swarm service or shared network is touched.
func TestCloneProvisionOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	pg, err := postgres.Run(ctx, "postgres:16-alpine", postgres.WithDatabase("postgres"), postgres.WithUsername("krill"), postgres.WithPassword("fixture-password"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(45*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, pg)
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	run := func(script, app string) error {
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "-e", "PGAPPNAME="+app, pg.GetContainerID(), "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "krill", "-d", "postgres")
		cmd.Stdin = strings.NewReader(script)
		// Never print SQL-bearing stderr, even for fixture passwords.
		return cmd.Run()
	}
	sql := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		t.Helper()
		var found bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_database WHERE datname=$1)", name).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	identity := func(n int) CloneDatabase {
		return CloneDatabase{Operation: fmt.Sprintf("%032x", n), DBName: fmt.Sprintf("clone_%d", n), Username: fmt.Sprintf("clone_user_%d", n), Deadline: time.Now().Add(time.Minute)}
	}
	provision := func(c CloneDatabase) string {
		t.Helper()
		s, err := c.provisionSQL("fixture_password_for_clone_123")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cleanup := func(c CloneDatabase) {
		t.Helper()
		s, err := c.cleanupSQL()
		if err != nil {
			t.Fatal(err)
		}
		if err := run(s, "cleanup"); err != nil {
			t.Fatalf("cleanup failed: %v", err)
		}
	}

	t.Run("empty isolated database and repeatable cleanup", func(t *testing.T) {
		sql("CREATE USER source_owner PASSWORD 'source_fixture_password'")
		sql("CREATE DATABASE source_db OWNER source_owner")
		sql("REVOKE CONNECT ON DATABASE source_db FROM PUBLIC")
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Database = "source_db"
		source, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		if _, err := source.Exec(ctx, "CREATE TABLE sentinel (value text); INSERT INTO sentinel VALUES ('production')"); err != nil {
			t.Fatal(err)
		}
		c := identity(1)
		if err := run(provision(c), "provision"); err != nil {
			t.Fatalf("provision failed: %v", err)
		}
		cfg.ConnConfig.Database, cfg.ConnConfig.User, cfg.ConnConfig.Password = c.DBName, c.Username, "fixture_password_for_clone_123"
		cloned, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := cloned.Ping(ctx); err != nil {
			t.Fatal("new credentials cannot connect")
		}
		var count int
		if err := cloned.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&count); err != nil || count != 0 {
			t.Fatal("clone is not empty")
		}
		cloned.Close()
		var allowed bool
		if err := pool.QueryRow(ctx, "SELECT has_database_privilege($1, 'source_db', 'CONNECT')", c.Username).Scan(&allowed); err != nil || allowed {
			t.Fatal("clone role can connect to source")
		}
		cleanup(c)
		cleanup(c)
		if exists(c.DBName) {
			t.Fatal("owned database survived cleanup")
		}
		var value string
		if err := source.QueryRow(ctx, "SELECT value FROM sentinel").Scan(&value); err != nil || value != "production" {
			t.Fatal("source sentinel was modified")
		}
	})

	t.Run("foreign role and database collisions survive", func(t *testing.T) {
		c := identity(2)
		sql("CREATE USER " + c.Username)
		sql("CREATE DATABASE " + c.DBName + " OWNER " + c.Username)
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Database = c.DBName
		foreign, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer foreign.Close()
		if _, err := foreign.Exec(ctx, "CREATE TABLE sentinel (value int); INSERT INTO sentinel VALUES (42)"); err != nil {
			t.Fatal(err)
		}
		if err := run(provision(c), "collision"); err == nil {
			t.Fatal("foreign role collision accepted")
		}
		cleanup(c)
		var value int
		if err := foreign.QueryRow(ctx, "SELECT value FROM sentinel").Scan(&value); err != nil || value != 42 {
			t.Fatal("foreign database was damaged")
		}
		// Also fail after the new role committed: a foreign database with a
		// different owner must survive while the newly owned role is removed.
		other := identity(3)
		other.DBName = c.DBName
		if err := run(provision(other), "db-collision"); err == nil {
			t.Fatal("foreign database collision accepted")
		}
		cleanup(other)
		if err := foreign.QueryRow(ctx, "SELECT value FROM sentinel").Scan(&value); err != nil || value != 42 {
			t.Fatal("foreign database owner check failed")
		}
	})

	t.Run("abort fences executor paused before first PG lock", func(t *testing.T) {
		c := identity(4)
		blocker, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock(8675309)"); err != nil {
			t.Fatal(err)
		}
		defer blocker.Exec(context.Background(), "SELECT pg_advisory_unlock(8675309)")
		// Represents an exec already launched by the now-cancelled/crashed
		// coordinator, but not yet entering its first operation lock.
		script := "SELECT pg_advisory_lock(8675309);\n" + provision(c)
		done := make(chan error, 1)
		go func() { done <- run(script, "late-clone-executor") }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_stat_activity WHERE application_name='late-clone-executor' AND wait_event='advisory')").Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("executor did not pause")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cleanup(c) // A new recovery invocation sees no objects but revokes first.
		if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock(8675309)"); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil {
			t.Fatal("late executor bypassed abort fence")
		}
		if exists(c.DBName) {
			t.Fatal("late executor created untracked database")
		}
		var roles int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_roles WHERE rolname=$1", c.Username).Scan(&roles); err != nil || roles != 0 {
			t.Fatal("late executor leaked a role")
		}
	})

	t.Run("expired fences can be pruned without authorizing old executor", func(t *testing.T) {
		c := identity(5)
		c.Deadline = time.Now().Add(-time.Minute)
		cleanup(c)
		if err := run(provision(c), "expired"); err == nil {
			t.Fatal("expired executor gained a fresh authorization")
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM krill_environment_clone.fences WHERE operation=$1", c.key()).Scan(&count); err != nil || count != 0 {
			t.Fatal("expired executor recreated fence metadata")
		}
	})

	t.Run("pruning watermark fences old executor even before wall clock expiry", func(t *testing.T) {
		c := identity(6)
		// Model the clock having moved backwards after a pruning pass. The
		// durable cutoff, not the current wall clock, must retain the denial.
		sql("INSERT INTO krill_environment_clone.fences VALUES ($1,$2,true)", c.key(), c.Deadline)
		script, err := cloneFencePruneSQL(c.Deadline.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := run(script, "prune-skewed-clock"); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM krill_environment_clone.fences WHERE operation=$1", c.key()).Scan(&count); err != nil || count != 0 {
			t.Fatal("lagging target clock stranded fence metadata")
		}
		if err := run(provision(c), "clock-rollback"); err == nil {
			t.Fatal("pruned authorization was resurrected after a clock rollback")
		}
		if exists(c.DBName) {
			t.Fatal("expired authorization created a database")
		}
	})
}
