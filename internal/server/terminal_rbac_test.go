package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestTerminalMemberForbidden(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "term-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "prod")
	a, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "t1.example.com", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	memberID := mkUser(t, q, "term-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie := loginAs(t, q, "term-member@k.local")

	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID) + "/terminal/ws"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member terminal want 403, got %d", rec.Code)
	}
}

func TestTerminalCrossTenantNotFound(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	userA := mkUser(t, q, "term-a@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userA, "OrgA")
	pA, _ := orgSvc.CreateProject(ctx, orgA.ID, "PA", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "prod-a")
	appA, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: eA.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "t2.example.com", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	userB := mkUser(t, q, "term-b@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userB, "OrgB")
	pB, _ := orgSvc.CreateProject(ctx, orgB.ID, "PB", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "prod-b")
	cookieB := loginAs(t, q, "term-b@k.local")

	// Org-B chain but org-A's appID -> chain-check mismatch -> 404.
	target := "/orgs/" + i64(orgB.ID) + "/projects/" + i64(pB.ID) + "/environments/" + i64(eB.ID) + "/apps/" + i64(appA.ID) + "/terminal/ws"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(cookieB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant terminal want 404, got %d", rec.Code)
	}
}
