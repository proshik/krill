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

func TestDetectEnvLinksLogical(t *testing.T) {
	env := map[string]string{"DATABASE_URL": "postgres://u:p@krill-postgres-pg1-x:5432/readeck"}
	insts := []EnvInstance{{ServiceID: "db-1", AppName: "krill-postgres-pg1-x", Engine: "postgres"}}
	logs := []EnvLogical{{ID: 9, InstanceAppName: "krill-postgres-pg1-x", DbName: "readeck"}}
	got := DetectEnvLinks(env, insts, logs)
	if len(got) != 1 || got[0].ToKind != "logical" || got[0].ToID != "9" || got[0].Engine != "postgres" || got[0].VarName != "DATABASE_URL" {
		t.Fatalf("want one logical detection to db 9, got %+v", got)
	}
}

func TestDetectEnvLinksLogicalBoundary(t *testing.T) {
	env := map[string]string{"DSN": "postgres://u:p@krill-postgres-pg1-x:5432/readeck?sslmode=disable"}
	insts := []EnvInstance{{ServiceID: "db-1", AppName: "krill-postgres-pg1-x", Engine: "postgres"}}
	// "read" is a prefix of "readeck" — must NOT be chosen; readeck must win regardless of slice order.
	logs := []EnvLogical{{ID: 5, InstanceAppName: "krill-postgres-pg1-x", DbName: "read"}, {ID: 9, InstanceAppName: "krill-postgres-pg1-x", DbName: "readeck"}}
	got := DetectEnvLinks(env, insts, logs)
	if len(got) != 1 || got[0].ToKind != "logical" || got[0].ToID != "9" {
		t.Fatalf("want logical db 9 (readeck), got %+v", got)
	}
}

func TestDetectEnvLinksInstanceAndRedisAndMiss(t *testing.T) {
	insts := []EnvInstance{
		{ServiceID: "db-1", AppName: "krill-postgres-pg1-x", Engine: "postgres"},
		{ServiceID: "db-2", AppName: "krill-redis-r-x", Engine: "redis"},
	}
	// postgres host present but no logical dbname match -> instance target
	pg := DetectEnvLinks(map[string]string{"DSN": "postgres://u:p@krill-postgres-pg1-x:5432/other"}, insts, nil)
	if len(pg) != 1 || pg[0].ToKind != "instance" || pg[0].ToID != "db-1" {
		t.Fatalf("want instance detection db-1, got %+v", pg)
	}
	// redis host -> redis instance
	rd := DetectEnvLinks(map[string]string{"R": "redis://default:pw@krill-redis-r-x:6379"}, insts, nil)
	if len(rd) != 1 || rd[0].ToKind != "instance" || rd[0].ToID != "db-2" || rd[0].Engine != "redis" {
		t.Fatalf("want redis detection db-2, got %+v", rd)
	}
	// unrelated host -> no detection
	if miss := DetectEnvLinks(map[string]string{"X": "postgres://u:p@some-other-host:5432/db"}, insts, nil); len(miss) != 0 {
		t.Fatalf("want no detection, got %+v", miss)
	}
}

func TestMergeLinksCollapseAndDedup(t *testing.T) {
	links := []LinkInput{
		{From: "app-1", ToKind: "logical", ToID: "10", Kind: "db", Engine: "postgres", VarName: "HOST"},
		{From: "app-1", ToKind: "logical", ToID: "10", Kind: "db", Engine: "postgres", VarName: "USER"},
		{From: "app-1", ToKind: "logical", ToID: "10", Kind: "db", Engine: "postgres", VarName: "PASSWORD"},
		{From: "app-1", ToKind: "logical", ToID: "10", Kind: "db", Detected: true, Engine: "postgres", VarName: "URL"},       // duplicate of modeled -> dropped
		{From: "app-2", ToKind: "instance", ToID: "db-3", Kind: "db", Detected: true, Engine: "redis", VarName: "REDIS_URL"}, // detected-only survives
	}
	got := MergeLinks(links)
	if len(got) != 2 {
		t.Fatalf("want 2 merged links, got %d: %+v", len(got), got)
	}
	var pg, rd *LinkInput
	for i := range got {
		if got[i].From == "app-1" {
			pg = &got[i]
		} else {
			rd = &got[i]
		}
	}
	if pg == nil || pg.Detected || pg.Label != "HOST, USER, PASSWORD" {
		t.Errorf("app-1 must be one modeled link with aggregated label, got %+v", pg)
	}
	if rd == nil || !rd.Detected {
		t.Errorf("app-2 detected-only link must survive, got %+v", rd)
	}
}

func TestBuildIngressAndServiceTarget(t *testing.T) {
	in := Inputs{
		Nodes: []NodeInput{{ID: "ingress", Name: "Ingress"}, {ID: "n1", Name: "control-plane"}},
		Services: []ServiceInput{
			{ID: "gateway", Kind: "gateway", NodeID: "ingress", Label: "Traefik"},
			{ID: "app-1", Kind: "app", NodeID: "n1", Label: "web"},
		},
		Links: []LinkInput{
			{From: "gateway", ToKind: "service", ToID: "app-1", Kind: "ingress", Label: "web.example.com"}, // ingress: no cross-node despite ingress!=n1
			{From: "gateway", ToKind: "service", ToID: "ghost", Kind: "ingress"},                           // dangling -> dropped
		},
	}
	g := Build(in)
	if len(g.Links) != 1 {
		t.Fatalf("want 1 ingress link (dangling dropped), got %d", len(g.Links))
	}
	l := g.Links[0]
	if l.Kind != "ingress" || l.CrossNode || l.Label != "web.example.com" || l.ToKind != "service" {
		t.Errorf("ingress link wrong: %+v", l)
	}
}
