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

func TestTopologyPageOwnerOK(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-page-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "topo-page-owner@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `class="k-topo"`) {
		t.Errorf("topology island missing from page HTML")
	}
	if !strings.Contains(rec.Body.String(), "/topology/data") {
		t.Errorf("island data-url missing from page HTML")
	}
}

func TestTopologyPageMemberForbidden(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-page-owner2@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memberID := mkUser(t, q, "topo-page-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie := loginAs(t, q, "topo-page-member@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member want 403, got %d", rec.Code)
	}
}

func TestTopologyDataIngress(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-ingress-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.example.com", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: app.ID, Host: "web.example.com", Tls: true, IsPrimary: true, Exposed: true, Paths: "",
	}); err != nil {
		t.Fatalf("create domain: %v", err)
	}

	cookie := loginAs(t, q, "topo-ingress-owner@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology/data", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var g topology.Graph
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	var hasGateway bool
	for _, s := range g.Services {
		if s.Kind == "gateway" {
			hasGateway = true
		}
	}
	if !hasGateway {
		t.Errorf("gateway service missing")
	}
	var ingress *topology.GLink
	for i := range g.Links {
		if g.Links[i].Kind == "ingress" && g.Links[i].From == "gateway" && g.Links[i].ToID == "app-"+i64(app.ID) {
			ingress = &g.Links[i]
		}
	}
	if ingress == nil || ingress.Label != "web.example.com" {
		t.Errorf("ingress link gateway->app with domain label missing, got %+v", ingress)
	}
}

func TestTopologyDataDetectedEnvLink(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-det-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg1", AppName: "krill-postgres-pg1-t9",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	ld, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: "readeck", DbName: "readeck", Username: "readeck", Password: "pw",
	})
	if err != nil {
		t.Fatalf("create logical db: %v", err)
	}
	// App connects via a RAW env DSN (no app_db_links row).
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "readeck", Image: "readeck", Tag: "latest",
		Domain: "r.example.com", Port: 8000, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
		EnvText: "READECK_DATABASE_SOURCE=postgres://u:p@krill-postgres-pg1-t9:5432/readeck",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	cookie := loginAs(t, q, "topo-det-owner@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology/data", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var g topology.Graph
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	var det *topology.GLink
	for i := range g.Links {
		if g.Links[i].From == "app-"+i64(app.ID) && g.Links[i].ToKind == "logical" && g.Links[i].ToID == i64(ld.ID) {
			det = &g.Links[i]
		}
	}
	if det == nil || !det.Detected || det.Engine != "postgres" {
		t.Errorf("detected env link app->logical readeck missing/incorrect, got %+v", det)
	}
}

func TestTopologyDataNonExposedNoIngress(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-ne-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "internal", Image: "nginx", Tag: "alpine",
		Domain: "int.example.com", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: app.ID, Host: "int.example.com", Tls: false, IsPrimary: true, Exposed: false, Paths: ""}); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	cookie := loginAs(t, q, "topo-ne-owner@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology/data", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var g topology.Graph
	json.Unmarshal(rec.Body.Bytes(), &g)
	for _, l := range g.Links {
		if l.Kind == "ingress" && l.ToID == "app-"+i64(app.ID) {
			t.Errorf("non-exposed app must have no ingress edge, got %+v", l)
		}
	}
}

func TestTopologyDataMultiDomainOneEdge(t *testing.T) {
	h, q, orgSvc := newServerWithNodesEngine(t, topoTestEngine())
	ctx := context.Background()
	ownerID := mkUser(t, q, "topo-md-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, _ := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "a.example.com", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	for _, host := range []string{"a.example.com", "b.example.com"} {
		if _, err := q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: app.ID, Host: host, Tls: true, IsPrimary: host == "a.example.com", Exposed: true, Paths: ""}); err != nil {
			t.Fatalf("create domain %s: %v", host, err)
		}
	}
	cookie := loginAs(t, q, "topo-md-owner@k.local")
	rec := getWithCookie(t, h, "/orgs/"+i64(o.ID)+"/topology/data", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var g topology.Graph
	json.Unmarshal(rec.Body.Bytes(), &g)
	n := 0
	var lbl string
	for _, l := range g.Links {
		if l.Kind == "ingress" && l.ToID == "app-"+i64(app.ID) {
			n++
			lbl = l.Label
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 ingress edge for a 2-domain app, got %d", n)
	}
	if !strings.Contains(lbl, "a.example.com") || !strings.Contains(lbl, "b.example.com") {
		t.Errorf("aggregated label must list both domains, got %q", lbl)
	}
}
