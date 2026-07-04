// Package server (internal test, not server_test): this file asserts on
// placementContainsNode, an unexported helper, so it can't live in the
// black-box server_test package.
package server

import (
	"context"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestListPinnedArtifactsByNode(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	pwHash, _ := auth.HashPassword("pw")
	owner, err := q.CreateUser(ctx, db.CreateUserParams{Email: "orphan@k.local", PasswordHash: pwHash})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	org, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O", Slug: "o", OwnerID: owner.ID})
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
	proj, err := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: org.ID, Name: "P", Slug: "p", Description: ""})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	env, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: proj.ID, Name: "production", Slug: "production"})
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: env.ID, Name: "pinned-app", Image: "nginx", Tag: "alpine",
		Domain: "pinned-app.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("application: %v", err)
	}
	if err := q.SetApplicationPlacement(ctx, db.SetApplicationPlacementParams{
		ID: app.ID, PlacementMode: "pin", PlacementNodes: "node-abc",
	}); err != nil {
		t.Fatalf("set placement: %v", err)
	}

	pinned, err := q.ListPinnedApplications(ctx)
	if err != nil {
		t.Fatalf("ListPinnedApplications: %v", err)
	}
	var found *db.ListPinnedApplicationsRow
	for i := range pinned {
		if pinned[i].ID == app.ID {
			found = &pinned[i]
		}
	}
	if found == nil {
		t.Fatalf("ListPinnedApplications did not return app %d, got %+v", app.ID, pinned)
	}
	if !strings.Contains(found.PlacementNodes, "node-abc") {
		t.Fatalf("PlacementNodes = %q, want to contain node-abc", found.PlacementNodes)
	}
	if !placementContainsNode(found.PlacementNodes, "node-abc") {
		t.Fatalf("placementContainsNode(%q, node-abc) = false, want true", found.PlacementNodes)
	}
}
