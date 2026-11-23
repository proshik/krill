package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

// The card of the release that runs now offered "restore this release". Only
// older eligible releases may be rolled back to.
func TestDeploymentsListNoRollbackToActive(t *testing.T) {
	secret.Init("test-key")
	t.Cleanup(func() { secret.Init("") })
	c := AppCtx{
		Org:     db.Organization{ID: 1},
		Project: db.Project{ID: 2},
		Env:     db.Environment{ID: 3},
		App:     db.Application{ID: 4, Status: "running", SourceType: "image"},
		Role:    "owner",
	}
	deps := []db.ListDeploymentSummariesByApplicationRow{
		{ID: 12, Status: "done", RollbackEligible: true, ImageTag: "app:v2"},
		{ID: 11, Status: "done", RollbackEligible: true, ImageTag: "app:v1"},
	}
	var buf bytes.Buffer
	if err := DeploymentsList(c, deps).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if strings.Contains(html, "/deployments/12/rollback") {
		t.Error("the active release offers a rollback to itself")
	}
	if !strings.Contains(html, "/deployments/11/rollback") {
		t.Error("the older eligible release lost its rollback")
	}
}
