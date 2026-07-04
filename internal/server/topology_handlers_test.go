package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/topology"
)

// topoNodesEngine reports a two-node cluster (a manager/leader + one worker) so
// apps without a running task fall back to the control-plane lane.
func topoTestEngine() nodesEngine {
	return nodesEngine{live: []docker.SwarmNode{
		{ID: "n1", Hostname: "cp", Role: "manager", State: "ready", Leader: true},
		{ID: "n2", Hostname: "worker-1", Role: "worker", State: "ready"},
	}}
}

func mkApp(t *testing.T, q *db.Queries, envID int64, name, domain string) {
	t.Helper()
	_, err := q.CreateApplication(context.Background(), db.CreateApplicationParams{
		EnvironmentID: envID, Name: name, Image: "nginx", Tag: "alpine",
		Domain: domain, Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application %s: %v", name, err)
	}
}

func TestTopologyDataOwnerOK(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	mkApp(t, q, e.ID, "web-app", "web.example.com")

	cookie := loginAs(t, q, "topo-owner@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology/data", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var g topology.Graph
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(g.Nodes) < 2 {
		t.Errorf("want >=2 cluster nodes, got %d", len(g.Nodes))
	}
	found := false
	for _, s := range g.Services {
		if s.Label == "web-app" && s.Kind == "app" {
			found = true
		}
	}
	if !found {
		t.Errorf("app 'web-app' missing from topology services")
	}
}

func TestTopologyDataMemberForbidden(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-owner2@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memberID := mkUser(t, q, "topo-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie := loginAs(t, q, "topo-member@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology/data", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member want 403, got %d", rec.Code)
	}
}

func TestTopologyDataCrossTenant(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	// Org A with app "app-a".
	userA := mkUser(t, q, "topo-a@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userA, "OrgA")
	pA, _ := orgSvc.CreateProject(ctx, orgA.ID, "ProjA", "")
	eA, _ := orgSvc.CreateEnvironment(ctx, pA.ID, "prod-a")
	mkApp(t, q, eA.ID, "app-a", "a.example.com")
	// Org B with app "app-b".
	userB := mkUser(t, q, "topo-b@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, userB, "OrgB")
	pB, _ := orgSvc.CreateProject(ctx, orgB.ID, "ProjB", "")
	eB, _ := orgSvc.CreateEnvironment(ctx, pB.ID, "prod-b")
	mkApp(t, q, eB.ID, "app-b", "b.example.com")

	// A non-member of org B is 404 on org B's topology (RequireOrgMember).
	cookieA := loginAs(t, q, "topo-a@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(orgB.ID)+"/topology/data", cookieA)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant want 404, got %d", rec.Code)
	}

	// Org B's owner sees only org B's data — org A's app name must not leak.
	cookieB := loginAs(t, q, "topo-b@k.local")
	rec = getWithCookie(t, h, "/orgs/"+i64(orgB.ID)+"/topology/data", cookieB)
	if rec.Code != http.StatusOK {
		t.Fatalf("org-B owner want 200, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "app-a") {
		t.Errorf("SECURITY: org A's app leaked into org B's topology: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "app-b") {
		t.Errorf("org B's own app missing from its topology")
	}
}
