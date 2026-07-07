package dbservice

import "testing"

func TestProxySpec(t *testing.T) {
	pg := proxySpec(Instance{ID: 7, Engine: "postgres", AppName: "krill-postgres-x", ExternalPort: p32(5433)}, "krill-net")
	if pg.Name != "krill-dbproxy-7" {
		t.Fatalf("name = %q", pg.Name)
	}
	if len(pg.Args) != 2 || pg.Args[0] != "TCP-LISTEN:5433,fork,reuseaddr" || pg.Args[1] != "TCP:krill-postgres-x:5432" {
		t.Fatalf("args = %v", pg.Args)
	}
	if len(pg.Constraints) != 1 || pg.Constraints[0] != "node.role==manager" {
		t.Fatalf("constraints = %v", pg.Constraints)
	}
	if len(pg.Ports) != 1 || pg.Ports[0].Published != 5433 || pg.Ports[0].Target != 5433 || pg.Ports[0].Mode != "host" {
		t.Fatalf("ports = %+v", pg.Ports)
	}
	if pg.Network != "krill-net" || pg.Replicas != 1 {
		t.Fatalf("network/replicas = %q/%d", pg.Network, pg.Replicas)
	}
	rd := proxySpec(Instance{ID: 8, Engine: "redis", AppName: "krill-redis-y", ExternalPort: p32(6380)}, "krill-net")
	if rd.Args[1] != "TCP:krill-redis-y:6379" {
		t.Fatalf("redis target = %q", rd.Args[1])
	}
}

func p32(v int32) *int32 { return &v }
