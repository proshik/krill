package server

import (
	"context"
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
			{ID: "app-1", Kind: "app", NodeID: "n-a", Label: "api", Sub: "shop / prod", Status: "running"},
			{ID: "app-2", Kind: "app", NodeID: "n-b", Label: "worker", Sub: "shop / prod", Status: "running"},
			{ID: "db-7", Kind: "db", NodeID: "n-b", Label: "main-db", Engine: "postgres", Status: "running"},
		},
		Dbs: []topology.GDB{{ID: 70, ServiceID: "db-7", Name: "shop", Env: "prod"}},
		Links: []topology.GLink{
			// api: only a DSN in its env values.
			{From: "app-1", ToKind: "logical", ToID: "70", Kind: "db", Detected: true},
			// worker: modeled and detected to the same server — one wire, not detected.
			{From: "app-2", ToKind: "logical", ToID: "70", Kind: "db"},
			{From: "app-2", ToKind: "instance", ToID: "db-7", Kind: "db", Detected: true},
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
	if len(api.To) != 1 || api.To[0] != "t-db-7" || len(api.Detected) != 1 || api.Detected[0] != "t-db-7" {
		t.Errorf("a DSN-only dependency must be drawn and marked detected: to=%v detected=%v", api.To, api.Detected)
	}
	if len(worker.To) != 1 || worker.To[0] != "t-db-7" || len(worker.Detected) != 0 {
		t.Errorf("a modeled link wins over a detected one to the same server, listed once: to=%v detected=%v", worker.To, worker.Detected)
	}
	if lanes["t-app-1"] != "n-a" || lanes["t-app-2"] != "n-b" || lanes["t-db-7"] != "n-b" {
		t.Errorf("workloads placed in the wrong lanes: %v", lanes)
	}
}
