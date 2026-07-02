package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestGetClusterNodeBySwarmID(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	_, err := q.CreateClusterNode(ctx, db.CreateClusterNodeParams{
		Name: "db-1", SshHost: "10.0.0.9", SshPort: 22, SshUser: "root",
		SshKey: "enc:key", HostKey: "hk",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// resolve swarm id (set on join)
	row, _ := q.GetClusterNode(ctx, mustID(t, pool, "db-1"))
	if err := q.SetClusterNodeSwarmID(ctx, db.SetClusterNodeSwarmIDParams{ID: row.ID, SwarmNodeID: "swarmXYZ"}); err != nil {
		t.Fatalf("set swarm id: %v", err)
	}

	got, err := q.GetClusterNodeBySwarmID(ctx, "swarmXYZ")
	if err != nil {
		t.Fatalf("lookup by swarm id: %v", err)
	}
	if got.Name != "db-1" || got.SshHost != "10.0.0.9" {
		t.Fatalf("wrong row: %+v", got)
	}

	if _, err := q.GetClusterNodeBySwarmID(ctx, "nonexistent"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing swarm id: want pgx.ErrNoRows, got %v", err)
	}
}

func mustID(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), "SELECT id FROM cluster_nodes WHERE name=$1", name).Scan(&id); err != nil {
		t.Fatalf("id lookup: %v", err)
	}
	return id
}
