package database

import (
	"context"
	"database/sql"
	"strings"
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

// TestMigration000029CrossEngineCollisionAndOrphanLinks covers the two
// riskiest paths in the conversion INSERTs that TestMigration000029ConvertsLegacyDBs
// doesn't exercise: a same-org, same-name collision that spans BOTH engines
// (db_instances.UNIQUE(organization_id, name) doesn't care which table a row
// came from, but the per-engine collision window function used to), and the
// orphan-link DELETE for app_db_links rows whose target legacy DB row no
// longer exists.
func TestMigration000029CrossEngineCollisionAndOrphanLinks(t *testing.T) {
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

	// Seed: one org with a non-colliding postgres db ("maindb") plus a
	// postgres db AND a redis db both named "cache" — the cross-engine
	// collision the old per-engine window functions couldn't see (each one
	// only partitioned over its own legacy table).
	var orgID, projID, envID, userID, appID, pgMainID, rdCacheID int64
	mustRow := func(q string, args ...any) int64 {
		var id int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		return id
	}
	userID = mustRow(`INSERT INTO users (email, password_hash) VALUES ('collide@k','x') RETURNING id`)
	orgID = mustRow(`INSERT INTO organizations (name, slug, owner_id) VALUES ('OC','oc',$1) RETURNING id`, userID)
	projID = mustRow(`INSERT INTO projects (organization_id, name, slug) VALUES ($1,'PC','pc') RETURNING id`, orgID)
	envID = mustRow(`INSERT INTO environments (project_id, name, slug) VALUES ($1,'prod','prod') RETURNING id`, projID)
	appID = mustRow(`INSERT INTO applications (environment_id, name, image, tag, domain, port, env_text, source_type, git_url, git_branch, dockerfile_path)
		VALUES ($1,'app','nginx','alpine','collide.example.com',80,'','image','','','') RETURNING id`, envID)

	pgMainID = mustRow(`INSERT INTO postgres_dbs (environment_id, name, app_name, database_name, database_user, database_password, image)
		VALUES ($1,'maindb','krill-postgres-maindb-cc1','app','postgres','pw','postgres:17') RETURNING id`, envID)
	mustRow(`INSERT INTO postgres_dbs (environment_id, name, app_name, database_name, database_user, database_password, image)
		VALUES ($1,'cache','krill-postgres-cache-cc2','app','postgres','pw','postgres:17') RETURNING id`, envID)
	rdCacheID = mustRow(`INSERT INTO redis_dbs (environment_id, name, app_name, password, image)
		VALUES ($1,'cache','krill-redis-cache-cc3','pw2','redis:7') RETURNING id`, envID)

	// One valid link (to the non-colliding postgres db) that must survive
	// the conversion, and one orphan link whose db_id points at nothing —
	// resolveDBLinkURL already skipped it pre-migration, so the conversion
	// must drop it rather than carry an unmapped row forward.
	mustRow(`INSERT INTO app_db_links (application_id, engine, db_id, var_name, scheme)
		VALUES ($1,'postgres',$2,'MAIN_URL','postgresql') RETURNING id`, appID, pgMainID)
	mustRow(`INSERT INTO app_db_links (application_id, engine, db_id, var_name, scheme)
		VALUES ($1,'redis',$2,'CACHE_URL','redis') RETURNING id`, appID, rdCacheID)
	mustRow(`INSERT INTO app_db_links (application_id, engine, db_id, var_name, scheme)
		VALUES ($1,'postgres',999999,'DEAD_URL','postgresql') RETURNING id`, appID)

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// Both "cache" instances survived the shared UNIQUE(organization_id,
	// name) constraint with distinct, engine-qualified names.
	rows, err := pool.Query(ctx, `SELECT engine, name FROM db_instances
		WHERE organization_id=$1 AND (name = 'cache' OR name LIKE 'cache-%')`, orgID)
	if err != nil {
		t.Fatalf("query cache instances: %v", err)
	}
	names := map[string]string{}
	for rows.Next() {
		var engine, name string
		if err := rows.Scan(&engine, &name); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		names[engine] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	pgName, ok := names["postgres"]
	if !ok {
		t.Fatalf("postgres 'cache' instance missing (collision dropped a row?)")
	}
	rdName, ok := names["redis"]
	if !ok {
		t.Fatalf("redis 'cache' instance missing (collision dropped a row?)")
	}
	if pgName == rdName {
		t.Fatalf("cross-engine collision not resolved: both instances ended up named %q", pgName)
	}
	if !strings.HasPrefix(pgName, "cache") || !strings.HasPrefix(rdName, "cache") {
		t.Fatalf("collision-suffixed names must still start with %q: pg=%q redis=%q", "cache", pgName, rdName)
	}

	// The non-colliding "maindb" instance keeps its exact original name
	// (no spurious suffix from the shared window function).
	var maindbName string
	if err := pool.QueryRow(ctx, `SELECT name FROM db_instances WHERE organization_id=$1 AND app_name=$2`,
		orgID, "krill-postgres-maindb-cc1").Scan(&maindbName); err != nil {
		t.Fatalf("maindb instance: %v", err)
	}
	if maindbName != "maindb" {
		t.Fatalf("non-colliding instance name changed: got %q want %q", maindbName, "maindb")
	}

	// The orphan link is gone, not carried forward as an unmapped row.
	var deadCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_db_links WHERE var_name='DEAD_URL'`).Scan(&deadCount); err != nil {
		t.Fatalf("dead link count: %v", err)
	}
	if deadCount != 0 {
		t.Fatalf("orphan link should have been dropped, found %d", deadCount)
	}

	// Only the two valid links remain, both rewired onto a new FK.
	var totalLinks int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_db_links WHERE application_id=$1`, appID).Scan(&totalLinks); err != nil {
		t.Fatalf("total link count: %v", err)
	}
	if totalLinks != 2 {
		t.Fatalf("expected exactly the 2 rewired links to remain, got %d", totalLinks)
	}
	var rewiredCount int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_db_links
		WHERE application_id=$1 AND (logical_database_id IS NOT NULL OR instance_id IS NOT NULL)`, appID).
		Scan(&rewiredCount); err != nil {
		t.Fatalf("rewired link count: %v", err)
	}
	if rewiredCount != 2 {
		t.Fatalf("expected both remaining links to be rewired onto a new FK, got %d", rewiredCount)
	}
}
