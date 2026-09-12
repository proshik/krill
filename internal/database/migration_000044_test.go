package database

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Docker Hub removed the minio/minio repository, so a MinIO instance that stores
// a Docker Hub reference can no longer be redeployed. Migration 000044 moves those
// references to quay.io and must leave every other image alone.
func TestMigration000044MovesMinIOImagesToQuay(t *testing.T) {
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
	if err := m.Migrate(43); err != nil {
		t.Fatalf("migrate to 43: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	var userID, orgID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('m@k','x') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO organizations (name, slug, owner_id) VALUES ('O','o',$1) RETURNING id`, userID).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	cases := []struct {
		name, engine, before, after string
	}{
		{"hub-latest", "minio", "minio/minio:latest", "quay.io/minio/minio:latest"},
		{"hub-release", "minio", "minio/minio:RELEASE.2024-01-16T16-07-38Z", "quay.io/minio/minio:RELEASE.2024-01-16T16-07-38Z"},
		{"hub-explicit-host", "minio", "docker.io/minio/minio", "quay.io/minio/minio"},
		{"already-quay", "minio", "quay.io/minio/minio:latest", "quay.io/minio/minio:latest"},
		{"lookalike-repo", "minio", "minio/minio-extra:1", "minio/minio-extra:1"},
		{"private-mirror", "minio", "registry.example.com/minio/minio:1", "registry.example.com/minio/minio:1"},
		{"postgres", "postgres", "postgres:17", "postgres:17"},
	}
	for _, c := range cases {
		if _, err := pool.Exec(ctx, `INSERT INTO db_instances (organization_id, engine, name, app_name, image, superuser_password)
			VALUES ($1, $2, $3, $4, $5, 'pw')`, orgID, c.engine, c.name, "krill-"+c.name, c.before); err != nil {
			t.Fatalf("seed %s: %v", c.name, err)
		}
	}

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	for _, c := range cases {
		var got string
		if err := pool.QueryRow(ctx, `SELECT image FROM db_instances WHERE name = $1`, c.name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", c.name, err)
		}
		if got != c.after {
			t.Errorf("%s: image = %q, want %q", c.name, got, c.after)
		}
	}
}
