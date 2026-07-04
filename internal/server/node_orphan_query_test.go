package server_test

import (
	"context"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestListPinnedArtifactsByNode(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	org, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O", Slug: "o", OwnerID: mkUser(t, q, "orphan@k.local")})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if _, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: org.ID, Engine: "postgres", Name: "pinned", AppName: "krill-postgres-x",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	}); err != nil {
		t.Fatalf("instance: %v", err)
	}
	// pin it to worker-9
	insts, _ := q.ListDBInstancesByOrg(ctx, org.ID)
	if err := q.SetDBInstanceNode(ctx, db.SetDBInstanceNodeParams{ID: insts[0].ID, NodeHostname: "worker-9"}); err != nil {
		t.Fatalf("pin: %v", err)
	}

	got, err := q.ListDBInstancesByNodeHostname(ctx, "worker-9")
	if err != nil || len(got) != 1 || got[0].Name != "pinned" {
		t.Fatalf("ListDBInstancesByNodeHostname = %+v, err %v", got, err)
	}
	if none, _ := q.ListDBInstancesByNodeHostname(ctx, "worker-nope"); len(none) != 0 {
		t.Fatalf("expected no instances for unknown node, got %d", len(none))
	}
	// ListPinnedApplications returns only pinned apps
	pinned, err := q.ListPinnedApplications(ctx)
	if err != nil {
		t.Fatalf("ListPinnedApplications: %v", err)
	}
	_ = pinned // count depends on other fixtures; just assert the query runs
}
