package drivers

import (
	"strings"
	"testing"
)

func p32(v int32) *int32 { return &v }

// --- Regression: driver BuildSpec must match the legacy dbservice.instanceSpec ---

func TestPostgresDriverSpecMatchesLegacy(t *testing.T) {
	inst := Instance{Engine: "postgres", AppName: "krill-pg-x", Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "pw"}
	spec := Registry.MustGet("postgres").BuildSpec(inst, "krill-net")
	if spec.Env["POSTGRES_USER"] != "postgres" || spec.Env["POSTGRES_PASSWORD"] != "pw" {
		t.Fatalf("pg env: %v", spec.Env)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Target != "/var/lib/postgresql/data" {
		t.Fatalf("pg mount: %+v", spec.Mounts)
	}
}

func TestRedisDriverSpec(t *testing.T) {
	inst := Instance{Engine: "redis", AppName: "krill-redis-y", Image: "redis:7", SuperuserPassword: "pw"}
	spec := Registry.MustGet("redis").BuildSpec(inst, "krill-net")
	want := []string{"redis-server", "--requirepass", "pw"}
	if len(spec.Args) != 3 || spec.Args[0] != want[0] || spec.Args[2] != "pw" {
		t.Fatalf("redis args: %v", spec.Args)
	}
}

func TestPostgresDriverSpecFull(t *testing.T) {
	inst := Instance{
		Engine: "postgres", AppName: "krill-postgres-x-abc123", Image: "postgres:17",
		Superuser: "postgres", SuperuserPassword: "pw", NodeHostname: "worker-1",
		ExternalPort: p32(55001),
	}
	s := Registry.MustGet("postgres").BuildSpec(inst, "krill-net")
	if s.Name != inst.AppName || s.Image != "postgres:17" || !s.DNSRR || s.Replicas != 1 {
		t.Fatalf("basic fields wrong: %+v", s)
	}
	if s.Network != "krill-net" {
		t.Fatalf("network wrong: %q", s.Network)
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
		t.Fatalf("DB service must not host-publish (external access goes via the proxy), got %+v", s.Ports)
	}
}

func TestRedisDriverSpecFull(t *testing.T) {
	inst := Instance{Engine: "redis", AppName: "krill-redis-y-abc123", Image: "redis:7", SuperuserPassword: "pw"}
	s := Registry.MustGet("redis").BuildSpec(inst, "krill-net")
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

// --- Registry ---

func TestRegistryGetAndList(t *testing.T) {
	if d, ok := Registry.Get("postgres"); !ok || d.Engine() != "postgres" {
		t.Fatalf("get postgres: %v %v", d, ok)
	}
	if d, ok := Registry.Get("redis"); !ok || d.Engine() != "redis" {
		t.Fatalf("get redis: %v %v", d, ok)
	}
	if d, ok := Registry.Get("dragonfly"); !ok || d.Engine() != "dragonfly" {
		t.Fatalf("get dragonfly: %v %v", d, ok)
	}
	if _, ok := Registry.Get("minio"); ok {
		t.Fatal("minio must not be registered yet")
	}
	list := Registry.List()
	if len(list) != 3 {
		t.Fatalf("List() = %d drivers, want 3", len(list))
	}
	if list[0].Engine() != "postgres" || list[1].Engine() != "redis" || list[2].Engine() != "dragonfly" {
		t.Fatalf("List() order = [%s, %s, %s], want stable [postgres, redis, dragonfly]", list[0].Engine(), list[1].Engine(), list[2].Engine())
	}
}

// --- DragonFly (Redis-compatible: RESP wire protocol, same link/conn shape) ---

func TestDragonflyDriverSpec(t *testing.T) {
	d, ok := Registry.Get("dragonfly")
	if !ok {
		t.Fatal("dragonfly driver not registered")
	}
	if d.Label() != "DragonFly" {
		t.Fatalf("label = %q", d.Label())
	}
	if !strings.Contains(d.DefaultImage(), "dragonfly") {
		t.Fatalf("default image = %q, want it to contain \"dragonfly\"", d.DefaultImage())
	}
	if d.SuperuserName() != "default" {
		t.Fatalf("superuser = %q, want \"default\"", d.SuperuserName())
	}
	if d.MountTarget() != "/data" {
		t.Fatalf("mount target = %q, want \"/data\"", d.MountTarget())
	}
	if d.HasLogicalResource() {
		t.Fatal("dragonfly must not have a logical resource")
	}

	inst := Instance{Engine: "dragonfly", AppName: "krill-dragonfly-x", Image: d.DefaultImage(), SuperuserPassword: "pw"}
	spec := d.BuildSpec(inst, "krill-net")
	wantArgs := []string{"dragonfly", "--requirepass", "pw"}
	if len(spec.Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", spec.Args, wantArgs)
	}
	for i, a := range wantArgs {
		if spec.Args[i] != a {
			t.Fatalf("args = %v, want %v", spec.Args, wantArgs)
		}
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Target != "/data" {
		t.Fatalf("mount: %+v", spec.Mounts)
	}
	if len(spec.Ports) != 0 {
		t.Fatalf("DB service must not host-publish, got %+v", spec.Ports)
	}

	targets := d.ExternalTargets(Instance{ExternalPort: p32(56501)})
	if len(targets) != 1 || targets[0].ContainerPort != 6379 || targets[0].Suffix != "" {
		t.Fatalf("external targets: %+v", targets)
	}

	wantFields := []string{"url", "password", "host", "port", "hostport"}
	if got := d.LinkFields(); len(got) != len(wantFields) {
		t.Fatalf("link fields = %v, want %v", got, wantFields)
	} else {
		for i, f := range wantFields {
			if got[i] != f {
				t.Fatalf("link fields = %v, want %v", got, wantFields)
			}
		}
	}

	src := LinkSource{AppName: "h", Password: "p", Scheme: "redis"}
	if got, ok := d.LinkValue(src, "url"); !ok || got != "redis://default:p@h:6379" {
		t.Errorf("dragonfly url = %q,%v", got, ok)
	}
	if got, ok := d.LinkValue(src, "hostport"); !ok || got != "h:6379" {
		t.Errorf("dragonfly hostport = %q,%v", got, ok)
	}
}

func TestDragonflyConnDisplay(t *testing.T) {
	inst := Instance{AppName: "df-inst", SuperuserPassword: "pw", ExternalPort: p32(56502)}
	fields := Registry.MustGet("dragonfly").ConnDisplay(inst, "example.com")
	if len(fields) != 2 {
		t.Fatalf("want 2 fields, got %+v", fields)
	}
	if fields[0].Value != "redis://default:pw@df-inst:6379" || !fields[0].Secret {
		t.Fatalf("internal: %+v", fields[0])
	}
	if fields[1].Value != "redis://default:pw@example.com:56502" || !fields[1].Secret {
		t.Fatalf("external: %+v", fields[1])
	}
}

// --- ExternalTargets ---

func TestExternalTargets(t *testing.T) {
	p := p32(55001)
	pgTargets := Registry.MustGet("postgres").ExternalTargets(Instance{ExternalPort: p})
	if len(pgTargets) != 1 || pgTargets[0].ContainerPort != 5432 || pgTargets[0].HostPort != p {
		t.Fatalf("pg targets: %+v", pgTargets)
	}
	rTargets := Registry.MustGet("redis").ExternalTargets(Instance{ExternalPort: p})
	if len(rTargets) != 1 || rTargets[0].ContainerPort != 6379 || rTargets[0].HostPort != p {
		t.Fatalf("redis targets: %+v", rTargets)
	}
}

// --- HasLogicalResource ---

func TestHasLogicalResource(t *testing.T) {
	if !Registry.MustGet("postgres").HasLogicalResource() {
		t.Fatal("postgres must have a logical resource")
	}
	if Registry.MustGet("redis").HasLogicalResource() {
		t.Fatal("redis must not have a logical resource")
	}
}

// --- LinkValue: mirrors the legacy dbLinkFieldValue table (internal/deploy/store.go) ---

func TestPostgresLinkValue(t *testing.T) {
	src := LinkSource{AppName: "h", Superuser: "u", Password: "p", DBName: "d", Scheme: "postgresql"}
	want := map[string]string{
		"url":      "postgresql://u:p@h:5432/d",
		"":         "postgresql://u:p@h:5432/d", // empty defaults to url
		"password": "p",
		"host":     "h",
		"port":     "5432",
		"user":     "u",
		"dbname":   "d",
		"hostport": "h:5432",
	}
	d := Registry.MustGet("postgres")
	for field, w := range want {
		got, ok := d.LinkValue(src, field)
		if !ok || got != w {
			t.Errorf("pg field %q = %q,%v want %q", field, got, ok, w)
		}
	}
}

func TestRedisLinkValue(t *testing.T) {
	// redis source (no dbname): url must omit the trailing /db, user is always "default".
	src := LinkSource{AppName: "h", Password: "p", Scheme: "redis"}
	d := Registry.MustGet("redis")
	if got, ok := d.LinkValue(src, "url"); !ok || got != "redis://default:p@h:6379" {
		t.Errorf("redis url = %q,%v", got, ok)
	}
	if got, ok := d.LinkValue(src, "password"); !ok || got != "p" {
		t.Errorf("redis password = %q,%v", got, ok)
	}
	if got, ok := d.LinkValue(src, "hostport"); !ok || got != "h:6379" {
		t.Errorf("redis hostport = %q,%v", got, ok)
	}
}

func TestLinkValueFreeFunctionUnknownEngine(t *testing.T) {
	if _, ok := LinkValue("minio", LinkSource{}, "url"); ok {
		t.Fatal("unknown engine must return ok=false")
	}
}

func TestLinkValueFreeFunctionDispatches(t *testing.T) {
	got, ok := LinkValue("postgres", LinkSource{AppName: "h", Superuser: "u", Password: "p", DBName: "d", Scheme: "postgresql"}, "url")
	if !ok || got != "postgresql://u:p@h:5432/d" {
		t.Fatalf("dispatch: %q,%v", got, ok)
	}
}

// --- ConnDisplay ---

func TestPostgresConnDisplay(t *testing.T) {
	inst := Instance{AppName: "pg-inst", Superuser: "postgres", SuperuserPassword: "su", ExternalPort: p32(55001)}
	fields := Registry.MustGet("postgres").ConnDisplay(inst, "example.com")
	if len(fields) != 2 {
		t.Fatalf("want 2 fields (internal+external), got %+v", fields)
	}
	if fields[0].Value != "postgresql://postgres:su@pg-inst:5432/postgres" || !fields[0].Secret {
		t.Fatalf("internal: %+v", fields[0])
	}
	if fields[1].Value != "postgresql://postgres:su@example.com:55001/postgres" || !fields[1].Secret {
		t.Fatalf("external: %+v", fields[1])
	}
}

func TestPostgresConnDisplayNoExternalPort(t *testing.T) {
	inst := Instance{AppName: "pg-inst", Superuser: "postgres", SuperuserPassword: "su"}
	fields := Registry.MustGet("postgres").ConnDisplay(inst, "example.com")
	if len(fields) != 1 {
		t.Fatalf("want 1 field (internal only), got %+v", fields)
	}
}

func TestRedisConnDisplay(t *testing.T) {
	inst := Instance{AppName: "rd-inst", SuperuserPassword: "pw", ExternalPort: p32(56001)}
	fields := Registry.MustGet("redis").ConnDisplay(inst, "example.com")
	if len(fields) != 2 {
		t.Fatalf("want 2 fields, got %+v", fields)
	}
	if fields[0].Value != "redis://default:pw@rd-inst:6379" {
		t.Fatalf("internal: %+v", fields[0])
	}
	if fields[1].Value != "redis://default:pw@example.com:56001" {
		t.Fatalf("external: %+v", fields[1])
	}
}

// --- Pure helpers ---

func TestDBConstraint(t *testing.T) {
	if c := DBConstraint(""); c != "node.role==manager" {
		t.Errorf("empty node = %q, want manager", c)
	}
	if c := DBConstraint("worker1"); c != "node.hostname==worker1" {
		t.Errorf("worker node = %q", c)
	}
}

func TestVolumeName(t *testing.T) {
	if v := VolumeName("krill-pg-x"); v != "krill-pg-x-data" {
		t.Errorf("volume name = %q", v)
	}
}
