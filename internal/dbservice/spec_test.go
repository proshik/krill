package dbservice

import "testing"

func TestPostgresSpec(t *testing.T) {
	pg := PostgresDB{AppName: "krill-pg-x", DatabaseName: "app", DatabaseUser: "user", DatabasePassword: "pw", Image: "postgres:17"}
	s := postgresSpec(pg, "krill-net")
	if s.Name != "krill-pg-x" || s.Image != "postgres:17" || !s.DNSRR || s.Replicas != 1 {
		t.Fatalf("base wrong: %+v", s)
	}
	if s.Env["POSTGRES_DB"] != "app" || s.Env["POSTGRES_USER"] != "user" || s.Env["POSTGRES_PASSWORD"] != "pw" {
		t.Errorf("env wrong: %+v", s.Env)
	}
	if len(s.Mounts) != 1 || s.Mounts[0].Type != "volume" || s.Mounts[0].Source != "krill-pg-x-data" || s.Mounts[0].Target != "/var/lib/postgresql/data" {
		t.Errorf("mount wrong: %+v", s.Mounts)
	}
	if len(s.Constraints) == 0 {
		t.Error("expected manager constraint")
	}
	if len(s.Ports) != 0 {
		t.Error("no external port => no published ports")
	}
}

func TestPostgresSpecExternalPort(t *testing.T) {
	port := int32(54320)
	pg := PostgresDB{AppName: "krill-pg-x", DatabaseName: "a", DatabaseUser: "u", DatabasePassword: "p", Image: "postgres:17", ExternalPort: &port}
	s := postgresSpec(pg, "krill-net")
	if len(s.Ports) != 1 || s.Ports[0].Target != 5432 || s.Ports[0].Published != 54320 || s.Ports[0].Mode != "host" {
		t.Errorf("external port wrong: %+v", s.Ports)
	}
}

func TestRedisSpec(t *testing.T) {
	r := RedisDB{AppName: "krill-redis-x", Password: "secret", Image: "redis:7"}
	s := redisSpec(r, "krill-net")
	if s.Name != "krill-redis-x" || !s.DNSRR {
		t.Fatalf("base wrong: %+v", s)
	}
	// Exec form: no shell, the password is a discrete argv element.
	if len(s.Command) != 0 {
		t.Errorf("command should be empty (use the image entrypoint), got %+v", s.Command)
	}
	if len(s.Args) != 3 || s.Args[0] != "redis-server" || s.Args[1] != "--requirepass" || s.Args[2] != "secret" {
		t.Errorf("args wrong: %+v", s.Args)
	}
	if len(s.Mounts) != 1 || s.Mounts[0].Target != "/data" || s.Mounts[0].Source != "krill-redis-x-data" {
		t.Errorf("mount wrong: %+v", s.Mounts)
	}
}

func TestConnectionStrings(t *testing.T) {
	port := int32(54320)
	pg := PostgresDB{AppName: "krill-pg-x", DatabaseName: "app", DatabaseUser: "user", DatabasePassword: "pw", ExternalPort: &port}
	if got := PostgresInternalURL(pg); got != "postgresql://user:pw@krill-pg-x:5432/app" {
		t.Errorf("pg internal = %q", got)
	}
	if got := PostgresExternalURL(pg, "1.2.3.4"); got != "postgresql://user:pw@1.2.3.4:54320/app" {
		t.Errorf("pg external = %q", got)
	}
	r := RedisDB{AppName: "krill-redis-x", Password: "secret", ExternalPort: &port}
	if got := RedisInternalURL(r); got != "redis://default:secret@krill-redis-x:6379" {
		t.Errorf("redis internal = %q", got)
	}
	if got := RedisExternalURL(r, "1.2.3.4"); got != "redis://default:secret@1.2.3.4:54320" {
		t.Errorf("redis external = %q", got)
	}
}
