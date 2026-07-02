package dbservice

import "testing"

func int32p(v int32) *int32 { return &v }

func TestInstanceSpecPostgres(t *testing.T) {
	inst := Instance{
		Engine: "postgres", AppName: "krill-postgres-x-abc123", Image: "postgres:17",
		Superuser: "postgres", SuperuserPassword: "pw", NodeHostname: "worker-1",
		ExternalPort: int32p(55001),
	}
	s := instanceSpec(inst, "krill-net")
	if s.Name != inst.AppName || s.Image != "postgres:17" || !s.DNSRR || s.Replicas != 1 {
		t.Fatalf("basic fields wrong: %+v", s)
	}
	if s.Env["POSTGRES_USER"] != "postgres" || s.Env["POSTGRES_PASSWORD"] != "pw" {
		t.Fatalf("env wrong: %+v", s.Env)
	}
	if _, has := s.Env["POSTGRES_DB"]; has {
		t.Fatal("POSTGRES_DB must not be set (databases are provisioned, not init-created)")
	}
	if len(s.Constraints) != 1 || s.Constraints[0] != "node.hostname==worker-1" {
		t.Fatalf("constraint wrong: %v", s.Constraints)
	}
	if len(s.Mounts) != 1 || s.Mounts[0].Source != "krill-postgres-x-abc123-data" || s.Mounts[0].Target != "/var/lib/postgresql/data" {
		t.Fatalf("mount wrong: %+v", s.Mounts)
	}
	if len(s.Ports) != 1 || s.Ports[0].Target != 5432 || s.Ports[0].Published != 55001 || s.Ports[0].Mode != "host" {
		t.Fatalf("ports wrong: %+v", s.Ports)
	}
}

func TestInstanceSpecRedis(t *testing.T) {
	inst := Instance{Engine: "redis", AppName: "krill-redis-y-abc123", Image: "redis:7", SuperuserPassword: "pw"}
	s := instanceSpec(inst, "krill-net")
	want := []string{"redis-server", "--requirepass", "pw"}
	if len(s.Args) != 3 || s.Args[0] != want[0] || s.Args[1] != want[1] || s.Args[2] != want[2] {
		t.Fatalf("args wrong: %v", s.Args)
	}
	if len(s.Constraints) != 1 || s.Constraints[0] != "node.role==manager" {
		t.Fatalf("default constraint wrong: %v", s.Constraints)
	}
	if s.Mounts[0].Target != "/data" {
		t.Fatalf("mount wrong: %+v", s.Mounts)
	}
}

func TestInstanceURLs(t *testing.T) {
	inst := Instance{AppName: "pg-inst", Superuser: "postgres", SuperuserPassword: "su", ExternalPort: int32p(55001)}
	ldb := LogicalDB{DBName: "shop", Username: "shop", Password: "pw"}
	if got := PostgresURL("postgresql", inst, ldb); got != "postgresql://shop:pw@pg-inst:5432/shop" {
		t.Fatalf("internal url: %s", got)
	}
	if got := PostgresExternalURL(inst, ldb, "example.com"); got != "postgresql://shop:pw@example.com:55001/shop" {
		t.Fatalf("external url: %s", got)
	}
	r := Instance{AppName: "rd-inst", SuperuserPassword: "pw", ExternalPort: int32p(56001)}
	if got := RedisInternalURL2(r); got != "redis://default:pw@rd-inst:6379" {
		t.Fatalf("redis internal: %s", got)
	}
	if got := RedisExternalURL2(r, "example.com"); got != "redis://default:pw@example.com:56001" {
		t.Fatalf("redis external: %s", got)
	}
}

func TestInstanceFeedID(t *testing.T) {
	if InstanceFeedID(3) != -3 {
		t.Fatalf("feed id: %d", InstanceFeedID(3))
	}
}
