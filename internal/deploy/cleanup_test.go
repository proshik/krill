package deploy_test

import (
	"context"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/testutil"
)

func TestClearOldDeploymentLogs(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	// minimal chain for FK applications → environment → project → org → user
	u, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "u@k", PasswordHash: "h"})
	o, _ := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "O", Slug: "o", OwnerID: u.ID})
	p, _ := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p"})
	e, _ := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "prod", Slug: "prod"})
	a, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine", Domain: "w.x", Port: 80,
		Env: map[string]string{}, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	d, _ := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: a.ID, Trigger: "manual"})
	// finish with a log and an artificially "old" finished_at
	_ = q.FinishDeployment(ctx, db.FinishDeploymentParams{
		ID: d.ID, Status: "done", ImageTag: "nginx:alpine", ErrorMessage: "", Log: "some build log",
	})
	// move finished_at back by 2 hours
	if _, err := pool.Exec(ctx, "UPDATE deployments SET finished_at = now() - interval '2 hours' WHERE id = $1", d.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	store := deploy.NewDBStore(q)
	if err := store.ClearOldDeploymentLogs(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ := q.GetDeployment(ctx, d.ID)
	if got.Log != "" {
		t.Errorf("log should be cleared, got %q", got.Log)
	}
	if got.Status != "done" {
		t.Errorf("record should remain (status done), got %q", got.Status)
	}

	// a fresh deployment with a log must NOT be cleared
	d2, _ := q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: a.ID, Trigger: "manual"})
	_ = q.FinishDeployment(ctx, db.FinishDeploymentParams{ID: d2.ID, Status: "done", ImageTag: "x", Log: "fresh"})
	_ = store.ClearOldDeploymentLogs(ctx)
	got2, _ := q.GetDeployment(ctx, d2.ID)
	if got2.Log != "fresh" {
		t.Errorf("fresh log must not be cleared, got %q", got2.Log)
	}
	_ = time.Now
}
