package server

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/topology"
	"github.com/proshik/krill/internal/web/templates"
)

// A database dependency found only in an app's env values must still be a
// wire on the map: the map draws data-to, data-detected only styles it.
// Two workers sharing a display label must keep their own workloads.
func TestTopoViewDetectedWiresAndSharedLabels(t *testing.T) {
	g := topology.Graph{
		Nodes: []topology.GNode{
			{ID: "ingress", Name: "ingress"},
			{ID: "n-a", Name: "west worker", State: "ready"},
			{ID: "n-b", Name: "west worker", State: "ready"},
		},
		Services: []topology.GService{
			{ID: "app-1", Kind: "app", NodeID: "n-a", Label: "api", Sub: "shop / prod", Status: "running", ProjectID: 1, Project: "shop"},
			{ID: "app-2", Kind: "app", NodeID: "n-b", Label: "worker", Sub: "shop / prod", Status: "running", ProjectID: 1, Project: "shop"},
			{ID: "db-7", Kind: "db", NodeID: "n-b", Label: "main-db", Engine: "postgres", Status: "running"},
		},
		Dbs: []topology.GDB{{ID: 70, ServiceID: "db-7", Name: "shop", Env: "prod"}, {ID: 71, ServiceID: "db-7", Name: "analytics", Env: "prod"}},
		Links: []topology.GLink{
			// api: only a DSN in its env values, to the shop database.
			{From: "app-1", ToKind: "logical", ToID: "70", Kind: "db", Detected: true},
			// worker: two modeled fields to analytics (one wire), and a DSN to shop.
			{From: "app-2", ToKind: "logical", ToID: "71", Kind: "db", Field: "url"},
			{From: "app-2", ToKind: "logical", ToID: "71", Kind: "db", Field: "password"},
			{From: "app-2", ToKind: "logical", ToID: "70", Kind: "db", Detected: true},
		},
	}
	v := topoView(context.Background(), 1, g, "")

	cards := map[string]templates.TopoCard{}
	lanes := map[string]string{}
	for _, l := range v.Lanes {
		for _, c := range append(append([]templates.TopoCard{}, l.Apps...), l.Servers...) {
			cards[c.ElemID] = c
			lanes[c.ElemID] = l.ID
		}
	}
	api, worker := cards["t-app-1"], cards["t-app-2"]
	if strings.Join(api.To, " ") != "t-ldb-70" || strings.Join(api.Detected, " ") != "t-ldb-70" {
		t.Errorf("a DSN-only dependency must be drawn to its database and marked detected: to=%v detected=%v", api.To, api.Detected)
	}
	if strings.Join(worker.To, " ") != "t-ldb-71 t-ldb-70" || strings.Join(worker.Detected, " ") != "t-ldb-70" {
		t.Errorf("each database is its own wire, fields to one database are one wire: to=%v detected=%v", worker.To, worker.Detected)
	}
	server := cards["t-db-7"]
	if len(server.Holds) != 2 || server.Holds[0].ElemID != "t-ldb-70" || server.Holds[1].ElemID != "t-ldb-71" {
		t.Errorf("the database chips must be wire ends: %+v", server.Holds)
	}
	if lanes["t-app-1"] != "n-a" || lanes["t-app-2"] != "n-b" || lanes["t-db-7"] != "n-b" {
		t.Errorf("workloads placed in the wrong lanes: %v", lanes)
	}
	for _, l := range v.Lanes {
		if l.Href != "" {
			t.Errorf("lane %s links to the Nodes page for someone who may not open it", l.Name)
		}
	}
}

// Projects are filtered by ID: "Sales" and "Sales / Europe" stay apart.
func TestTopoViewProjectsByID(t *testing.T) {
	g := topology.Graph{
		Nodes: []topology.GNode{{ID: "n1", Name: "control-plane", State: "ready"}},
		Services: []topology.GService{
			{ID: "app-1", Kind: "app", NodeID: "n1", Label: "a", Sub: "Sales / prod", ProjectID: 1, Project: "Sales"},
			{ID: "app-2", Kind: "app", NodeID: "n1", Label: "b", Sub: "Sales / Europe / prod", ProjectID: 2, Project: "Sales / Europe"},
		},
	}
	all := topoView(context.Background(), 1, g, "")
	if len(all.Projects) != 2 || all.Projects[0] != (templates.TopoProject{ID: "1", Name: "Sales"}) || all.Projects[1] != (templates.TopoProject{ID: "2", Name: "Sales / Europe"}) {
		t.Fatalf("projects: %+v", all.Projects)
	}
	one := topoView(context.Background(), 1, g, "2")
	var names []string
	for _, l := range one.Lanes {
		for _, c := range l.Apps {
			names = append(names, c.Name)
		}
	}
	if strings.Join(names, " ") != "b" {
		t.Fatalf("the filter for Sales / Europe shows %v", names)
	}
}

// Narrowed to a project, the org topology showed main-db with another
// project's database chips, and dropped the S3 server the project used
// through a bucket link (the link was never drawn). A server stays when a
// link of the project reaches it, and shows only that project's chips.
func TestTopoViewProjectFilterServers(t *testing.T) {
	g := topology.Graph{
		Nodes: []topology.GNode{{ID: "n1", Name: "control-plane", State: "ready"}},
		Services: []topology.GService{
			{ID: "app-1", Kind: "app", NodeID: "n1", Label: "shop-api", ProjectID: 1, Project: "shop"},
			{ID: "app-2", Kind: "app", NodeID: "n1", Label: "blog", ProjectID: 2, Project: "blog"},
			{ID: "db-1", Kind: "db", NodeID: "n1", Label: "main-db", Engine: "postgres"},
			{ID: "db-2", Kind: "db", NodeID: "n1", Label: "files", Engine: "minio"},
		},
		Dbs: []topology.GDB{
			{ID: 10, ServiceID: "db-1", Name: "shop", ProjectID: 1},
			{ID: 11, ServiceID: "db-1", Name: "blog", ProjectID: 2},
		},
		Links: []topology.GLink{
			{From: "app-1", ToKind: "logical", ToID: "10", Kind: "db"},
			{From: "app-2", ToKind: "logical", ToID: "11", Kind: "db"},
			{From: "app-1", ToKind: "instance", ToID: "db-2", Kind: "db", Engine: "minio"},
			// A raw DSN of shop-api pointing at the blog project's database.
			{From: "app-1", ToKind: "logical", ToID: "11", Kind: "db", Detected: true},
		},
	}
	v := topoView(context.Background(), 1, g, "1")
	for _, l := range v.Lanes {
		for _, c := range l.Apps {
			if c.Name == "shop-api" && !slices.Contains(c.Detected, "t-db-1") {
				t.Errorf("the DSN to another project's database must end at its server card, detected = %v", c.Detected)
			}
		}
	}
	servers := map[string][]string{}
	for _, l := range v.Lanes {
		for _, c := range l.Servers {
			for _, h := range c.Holds {
				servers[c.Name] = append(servers[c.Name], h.Name)
			}
			if _, ok := servers[c.Name]; !ok {
				servers[c.Name] = nil
			}
		}
	}
	if got := strings.Join(servers["main-db"], ","); got != "shop" {
		t.Errorf("main-db chips under the shop filter = %q, want shop", got)
	}
	if _, ok := servers["files"]; !ok {
		t.Errorf("the S3 server shop uses through a bucket link is missing: %v", servers)
	}
}
