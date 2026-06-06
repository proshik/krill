package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// TestAppCrossTenantIsolation verifies that an application owned by org A is not
// reachable through org B's route tree, even when the org-A application ID is
// supplied. The tenancy chain-check (loadOrg → loadProject → loadEnvironment →
// loadApp) must return 404 on every mismatch.
func TestAppCrossTenantIsolation(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	// --- Org-A: owner userA, project, environment, application ---
	userAID := mkUser(t, q, "appusera@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userAID, "OrgA")
	projA, _ := orgSvc.CreateProject(ctx, orgA.ID, "ProjA", "")
	envA, _ := orgSvc.CreateEnvironment(ctx, projA.ID, "prod-a")

	appA, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: envA.ID, Name: "app-a", Image: "nginx", Tag: "alpine",
		Domain: "app-a.x", Port: 80, Env: map[string]string{}, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create org-A application: %v", err)
	}
	aAppID := appA.ID

	// --- Org-B: owner userB, project, environment ---
	userBID := mkUser(t, q, "appuserb@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userBID, "OrgB")
	projB, _ := orgSvc.CreateProject(ctx, orgB.ID, "ProjB", "")
	envB, _ := orgSvc.CreateEnvironment(ctx, projB.ID, "prod-b")

	cookieB := loginAs(t, q, "appuserb@k.local")

	// Base path: org-B's chain but with org-A's appID.
	basePath := "/orgs/" + i64(orgB.ID) +
		"/projects/" + i64(projB.ID) +
		"/environments/" + i64(envB.ID) +
		"/apps/" + i64(aAppID)

	cases := []struct {
		method string
		suffix string
	}{
		{http.MethodGet, ""},
		{http.MethodPost, "/deploy"},
		{http.MethodPost, "/stop"},
		{http.MethodPost, "/env"},
	}

	for _, tc := range cases {
		target := basePath + tc.suffix
		req := httptest.NewRequest(tc.method, target, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookieB)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			// Any non-404 is a security finding — report loudly.
			t.Errorf("SECURITY FINDING: %s %s — expected 404 (cross-tenant isolation), got %d", tc.method, target, rec.Code)
		}
	}

	// The org-A application row must still exist after all cross-tenant attempts.
	if _, err := q.GetApplication(ctx, aAppID); err != nil {
		t.Fatalf("SECURITY FINDING: org-A application row (id=%d) was affected by cross-tenant request: %v", aAppID, err)
	}
}
