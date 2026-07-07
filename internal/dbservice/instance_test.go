package dbservice

import (
	"context"
	"io"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

func int32p(v int32) *int32 { return &v }

type recEngine struct {
	docker.Engine // embed nil: only the methods we call are implemented below
	deployed      []string
	removed       []string
}

func (e *recEngine) ImagePull(ctx context.Context, ref string, out io.Writer) error { return nil }
func (e *recEngine) ServiceDeploy(ctx context.Context, spec docker.ServiceSpec) error {
	e.deployed = append(e.deployed, spec.Name)
	return nil
}
func (e *recEngine) ServiceRemove(ctx context.Context, name string) error {
	e.removed = append(e.removed, name)
	return nil
}

func TestInstanceSpecNoHostPublish(t *testing.T) {
	spec := instanceSpec(Instance{Engine: "postgres", AppName: "krill-postgres-x", ExternalPort: p32(5433)}, "krill-net")
	if len(spec.Ports) != 0 {
		t.Fatalf("DB service must not host-publish external_port anymore, got %+v", spec.Ports)
	}

	redisSpec := instanceSpec(Instance{Engine: "redis", AppName: "krill-redis-y", ExternalPort: p32(6380)}, "krill-net")
	if len(redisSpec.Ports) != 0 {
		t.Fatalf("redis service must not host-publish external_port anymore, got %+v", redisSpec.Ports)
	}
}

func TestReconcileProxyDeployAndRemove(t *testing.T) {
	e := &recEngine{}
	s := &Service{engine: e, network: "krill-net"}
	if err := s.reconcileProxy(context.Background(), Instance{ID: 7, Engine: "postgres", AppName: "a", ExternalPort: p32(5433)}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(e.deployed) != 1 || e.deployed[0] != "krill-dbproxy-7" {
		t.Fatalf("deployed = %v", e.deployed)
	}
	if err := s.reconcileProxy(context.Background(), Instance{ID: 7, Engine: "postgres", AppName: "a", ExternalPort: nil}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(e.removed) != 1 || e.removed[0] != "krill-dbproxy-7" {
		t.Fatalf("removed = %v", e.removed)
	}
}

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
	if len(s.Ports) != 0 {
		t.Fatalf("DB service must not host-publish anymore (external access goes via the proxy), got %+v", s.Ports)
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
	if got := RedisInternalURL(r); got != "redis://default:pw@rd-inst:6379" {
		t.Fatalf("redis internal: %s", got)
	}
	if got := RedisExternalURL(r, "example.com"); got != "redis://default:pw@example.com:56001" {
		t.Fatalf("redis external: %s", got)
	}
}

func TestInstanceFeedID(t *testing.T) {
	if InstanceFeedID(3) != -3 {
		t.Fatalf("feed id: %d", InstanceFeedID(3))
	}
}
