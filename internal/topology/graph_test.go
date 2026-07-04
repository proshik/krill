package topology

import "testing"

func TestResolveNodesRunningOnly(t *testing.T) {
	got := ResolveNodes([]Task{
		{Service: "krill-1", Node: "n1", Running: true},
		{Service: "krill-1", Node: "n1", Running: true}, // duplicate node
		{Service: "krill-1", Node: "n2", Running: true},
		{Service: "krill-2", Node: "n1", Running: false}, // not running
		{Service: "", Node: "n1", Running: true},         // no service name
	})
	if len(got["krill-1"]) != 2 {
		t.Fatalf("krill-1 want 2 distinct running nodes, got %v", got["krill-1"])
	}
	if _, ok := got["krill-2"]; ok {
		t.Errorf("krill-2 has no running task, must be absent from the map")
	}
}

func TestBuildCrossNodeAndDangling(t *testing.T) {
	in := Inputs{
		Nodes: []NodeInput{{ID: "n1", Name: "control-plane"}, {ID: "n2", Name: "worker-1"}},
		Services: []ServiceInput{
			{ID: "app-1", Kind: "app", NodeID: "n1", Label: "web"},
			{ID: "db-1", Kind: "db", NodeID: "n2", Label: "pg", Engine: "postgres"},
			{ID: "db-2", Kind: "db", NodeID: "n1", Label: "cache", Engine: "redis"},
		},
		Dbs: []DBInput{{ID: 10, ServiceID: "db-1", Name: "app_db", NodeID: "n2"}},
		Links: []LinkInput{
			{From: "app-1", ToKind: "logical", ToID: "10", Engine: "postgres", VarName: "DATABASE_URL", Field: "url"}, // n1 -> n2 = cross-node
			{From: "app-1", ToKind: "instance", ToID: "db-2", Engine: "redis", VarName: "REDIS_URL", Field: "url"},    // n1 -> n1 = same node
			{From: "app-1", ToKind: "logical", ToID: "999", Engine: "postgres"},                                       // dangling: no db 999
			{From: "ghost", ToKind: "instance", ToID: "db-2"},                                                         // dangling: no source
		},
	}
	g := Build(in)
	if len(g.Links) != 2 {
		t.Fatalf("want 2 links after dropping 2 dangling, got %d", len(g.Links))
	}
	var pg, rd *GLink
	for i := range g.Links {
		switch g.Links[i].Engine {
		case "postgres":
			pg = &g.Links[i]
		case "redis":
			rd = &g.Links[i]
		}
	}
	if pg == nil || !pg.CrossNode {
		t.Errorf("postgres link (n1->n2) must be cross-node")
	}
	if rd == nil || rd.CrossNode {
		t.Errorf("redis link (n1->n1) must NOT be cross-node")
	}
	if len(g.Services) != 3 || len(g.Nodes) != 2 || len(g.Dbs) != 1 {
		t.Errorf("passthrough counts wrong: svc=%d node=%d db=%d", len(g.Services), len(g.Nodes), len(g.Dbs))
	}
}
