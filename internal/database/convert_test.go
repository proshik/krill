package database

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// newMigrator builds a migrate instance over the embedded FS (same wiring as
// RunMigrations, but stoppable at an arbitrary version).
func newMigrator(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("iofs: %v", err)
	}
	sqlDB, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	driver, err := migratepg.WithInstance(sqlDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return m
}

func TestMigration000029ConvertsLegacyDBs(t *testing.T) {
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

	m := newMigrator(t, dsn)
	if err := m.Migrate(28); err != nil {
		t.Fatalf("migrate to 28: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Seed the legacy shape: org → project → env, a postgres DB, a redis DB,
	// an app with links to both, and a backup on the postgres DB.
	var orgID, projID, envID, userID, pgID, rdID, appID, destID int64
	mustRow := func(q string, args ...any) int64 {
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	userID = mustRow(`INSERT INTO users (email, password_hash) VALUES ('c@k','x') RETURNING id`)
	orgID = mustRow(`INSERT INTO organizations (name, slug, owner_id) VALUES ('O','o',$1) RETURNING id`, userID)
	projID = mustRow(`INSERT INTO projects (organization_id, name, slug) VALUES ($1,'P','p') RETURNING id`, orgID)
	envID = mustRow(`INSERT INTO environments (project_id, name, slug) VALUES ($1,'prod','prod') RETURNING id`, projID)
	pgID = mustRow(`INSERT INTO postgres_dbs (environment_id, name, app_name, database_name, database_user, database_password, image, external_port, node_hostname)
		VALUES ($1,'maindb','krill-postgres-maindb-abc123','app','postgres','encpw','postgres:17',55001,'worker-1') RETURNING id`, envID)
	rdID = mustRow(`INSERT INTO redis_dbs (environment_id, name, app_name, password, image)
		VALUES ($1,'cache','krill-redis-cache-abc123','encpw2','redis:7') RETURNING id`, envID)
	appID = mustRow(`INSERT INTO applications (environment_id, name, image, tag, domain, port, env_text, source_type, git_url, git_branch, dockerfile_path)
		VALUES ($1,'app','nginx','alpine','app.example.com',80,'','image','','','') RETURNING id`, envID)
	mustRow(`INSERT INTO app_db_links (application_id, engine, db_id, var_name, scheme)
		VALUES ($1,'postgres',$2,'DATABASE_URL','postgresql') RETURNING id`, appID, pgID)
	mustRow(`INSERT INTO app_db_links (application_id, engine, db_id, var_name, scheme)
		VALUES ($1,'redis',$2,'REDIS_URL','redis') RETURNING id`, appID, rdID)
	destID = mustRow(`INSERT INTO destinations (organization_id, name, bucket, access_key, secret_key)
		VALUES ($1,'s3','bkt','ak','sk') RETURNING id`, orgID)
	mustRow(`INSERT INTO backups (postgres_db_id, destination_id, schedule, retention)
		VALUES ($1,$2,'0 3 * * *',7) RETURNING id`, pgID, destID)

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// Postgres container → instance with superuser creds + one logical DB.
	var instID, cnt int64
	var su, supw, appName, node string
	if err := pool.QueryRow(ctx, `SELECT id, superuser, superuser_password, app_name, node_hostname
		FROM db_instances WHERE engine='postgres' AND organization_id=$1`, orgID).
		Scan(&instID, &su, &supw, &appName, &node); err != nil {
		t.Fatalf("converted pg instance: %v", err)
	}
	if su != "postgres" || supw != "encpw" || appName != "krill-postgres-maindb-abc123" || node != "worker-1" {
		t.Fatalf("pg instance fields wrong: %s %s %s %s", su, supw, appName, node)
	}
	var ldbID int64
	var dbName, uname string
	if err := pool.QueryRow(ctx, `SELECT id, db_name, username FROM logical_databases
		WHERE instance_id=$1 AND environment_id=$2`, instID, envID).Scan(&ldbID, &dbName, &uname); err != nil {
		t.Fatalf("converted logical db: %v", err)
	}
	if dbName != "app" || uname != "postgres" {
		t.Fatalf("logical db fields wrong: %s %s", dbName, uname)
	}

	// Redis container → instance (no logical DBs).
	var rdInstID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM db_instances WHERE engine='redis' AND organization_id=$1`, orgID).
		Scan(&rdInstID); err != nil {
		t.Fatalf("converted redis instance: %v", err)
	}

	// Links rewired onto the new FKs.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_db_links
		WHERE application_id=$1 AND logical_database_id=$2`, appID, ldbID).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("pg link not rewired (err=%v cnt=%d)", err, cnt)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_db_links
		WHERE application_id=$1 AND instance_id=$2`, appID, rdInstID).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("redis link not rewired (err=%v cnt=%d)", err, cnt)
	}

	// Backup rewired onto the logical DB.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM backups
		WHERE logical_database_id=$1`, ldbID).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("backup not rewired (err=%v cnt=%d)", err, cnt)
	}
}
