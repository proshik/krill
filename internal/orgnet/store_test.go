package orgnet_test

import (
	"context"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/orgnet"
	"github.com/proshik/krill/internal/testutil"
)

// The migration decides whether an organization moved from these statuses, so
// the real query must return every listed deployment's status and omit ids that
// do not exist (an app deleted mid-pass).
func TestDBStoreDeploymentStatuses(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "u@k", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O", Slug: "o", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	p, err := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	e, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "prod", Slug: "prod"})
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "w.x", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("application: %v", err)
	}
	running, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: a.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	failed, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: a.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	if err := q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: failed.ID, Status: "error", ErrorMessage: "update rolled back"}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	missing := failed.ID + 1000
	got, err := orgnet.NewDBStore(q).DeploymentStatuses(ctx, []int64{running.ID, failed.ID, missing})
	if err != nil {
		t.Fatalf("statuses: %v", err)
	}
	if got[running.ID] != "running" || got[failed.ID] != "error" {
		t.Fatalf("statuses = %v", got)
	}
	if _, ok := got[missing]; ok || len(got) != 2 {
		t.Fatalf("a deployment that does not exist must be absent, got %v", got)
	}
}
