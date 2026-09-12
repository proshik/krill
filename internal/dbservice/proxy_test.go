package dbservice

import (
	"context"
	"errors"
	"testing"

	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
)

func TestProxySpecTarget(t *testing.T) {
	pg := proxySpecTarget(proxyName(7), 5433, "krill-postgres-x", 5432, "krill-net")
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
	rd := proxySpecTarget(proxyName(8), 6380, "krill-redis-y", 6379, "krill-net")
	if rd.Args[1] != "TCP:krill-redis-y:6379" {
		t.Fatalf("redis target = %q", rd.Args[1])
	}
}

func p32(v int32) *int32 { return &v }

// --- twoTargetDriver: a minimal test-only Driver stub simulating a future
// N-target engine (e.g. minio: S3 API + console) — exercises reconcileProxy's
// per-target loop ahead of the real minio driver (a later task) landing.
// Registered into the shared drivers.Registry once for this test binary
// (package dbservice's tests run in their own process, so this can't leak
// into internal/dbservice/drivers' own tests).

type twoTargetDriver struct{}

func (twoTargetDriver) Engine() string        { return "test2target" }
func (twoTargetDriver) Label() string         { return "Test2Target" }
func (twoTargetDriver) DefaultImage() string  { return "busybox" }
func (twoTargetDriver) SuperuserName() string { return "" }
func (twoTargetDriver) MountTarget() string   { return "/data" }
func (twoTargetDriver) BuildSpec(inst drivers.Instance, network string) docker.ServiceSpec {
	return docker.ServiceSpec{Name: inst.AppName, Network: network}
}
func (twoTargetDriver) ExternalTargets(inst drivers.Instance) []drivers.ProxyTarget {
	return []drivers.ProxyTarget{
		{Suffix: "", HostPort: inst.ExternalPort, ContainerPort: 9000},
		{Suffix: "-console", HostPort: inst.ConsoleExternalPort, ContainerPort: 9001},
	}
}
func (twoTargetDriver) HasLogicalResource() bool { return false }
func (twoTargetDriver) LinkFields() []string     { return nil }
func (twoTargetDriver) LinkValue(drivers.LinkSource, string) (string, bool) {
	return "", false
}
func (twoTargetDriver) ConnDisplay(drivers.Instance, string) []drivers.ConnField { return nil }

func init() {
	drivers.Registry.Register(twoTargetDriver{})
}

// TestReconcileProxyDeploysOnePerTargetWithHostPort is the RED test driving
// the N-target generalization: a driver with two ExternalTargets (both with a
// HostPort set) must deploy two distinct proxy services, one per target,
// named by the target's Suffix.
func TestReconcileProxyDeploysOnePerTargetWithHostPort(t *testing.T) {
	eng := newMockEngine()
	svc := newSvc(eng, newFakeStore(Instance{}))
	inst := Instance{ID: 42, Engine: "test2target", AppName: "krill-t2t-x", ExternalPort: p32(9100), ConsoleExternalPort: p32(9101)}
	if err := svc.reconcileProxy(context.Background(), inst); err != nil {
		t.Fatalf("reconcileProxy: %v", err)
	}
	if len(eng.deployed) != 2 {
		t.Fatalf("want 2 proxy services deployed, got %d: %+v", len(eng.deployed), eng.deployed)
	}
	byName := map[string]docker.ServiceSpec{}
	for _, d := range eng.deployed {
		byName[d.Name] = d
	}
	primary, ok := byName["krill-dbproxy-42"]
	if !ok {
		t.Fatalf("missing primary proxy service, deployed=%+v", eng.deployed)
	}
	if len(primary.Args) != 2 || primary.Args[0] != "TCP-LISTEN:9100,fork,reuseaddr" || primary.Args[1] != "TCP:krill-t2t-x:9000" {
		t.Fatalf("primary args = %v", primary.Args)
	}
	console, ok := byName["krill-dbproxy-42-console"]
	if !ok {
		t.Fatalf("missing console proxy service, deployed=%+v", eng.deployed)
	}
	if len(console.Args) != 2 || console.Args[0] != "TCP-LISTEN:9101,fork,reuseaddr" || console.Args[1] != "TCP:krill-t2t-x:9001" {
		t.Fatalf("console args = %v", console.Args)
	}
}

// TestReconcileProxyNilTargetHostPortRemovesJustThatProxy verifies that a
// target whose HostPort is nil (console disabled) removes only its own proxy
// service — the other target (with a HostPort) is deployed independently.
func TestReconcileProxyNilTargetHostPortRemovesJustThatProxy(t *testing.T) {
	eng := newMockEngine()
	svc := newSvc(eng, newFakeStore(Instance{}))
	inst := Instance{ID: 43, Engine: "test2target", AppName: "krill-t2t-y", ExternalPort: p32(9200), ConsoleExternalPort: nil}
	if err := svc.reconcileProxy(context.Background(), inst); err != nil {
		t.Fatalf("reconcileProxy: %v", err)
	}
	if len(eng.deployed) != 1 || eng.deployed[0].Name != "krill-dbproxy-43" {
		t.Fatalf("want 1 deployed (primary only), got %+v", eng.deployed)
	}
	if len(eng.removed) != 1 || eng.removed[0] != "krill-dbproxy-43-console" {
		t.Fatalf("want console proxy removed, got %+v", eng.removed)
	}
}

// TestReconcileProxySingleTargetUnchangedForPostgresAndRedis is the
// regression guard: engines whose driver returns exactly one ExternalTargets
// entry (postgres, redis — today's shape) must still deploy exactly one proxy
// service, unaffected by the N-target generalization.
func TestReconcileProxySingleTargetUnchangedForPostgresAndRedis(t *testing.T) {
	eng := newMockEngine()
	svc := newSvc(eng, newFakeStore(Instance{}))
	pg := samplePGInstance()
	pg.ExternalPort = p32(5433)
	if err := svc.reconcileProxy(context.Background(), pg); err != nil {
		t.Fatalf("pg reconcileProxy: %v", err)
	}
	if len(eng.deployed) != 1 {
		t.Fatalf("pg: want exactly 1 proxy deployed, got %d: %+v", len(eng.deployed), eng.deployed)
	}

	eng2 := newMockEngine()
	svc2 := newSvc(eng2, newFakeStore(Instance{}))
	rd := sampleRedisInstance()
	rd.ExternalPort = p32(6380)
	if err := svc2.reconcileProxy(context.Background(), rd); err != nil {
		t.Fatalf("redis reconcileProxy: %v", err)
	}
	if len(eng2.deployed) != 1 {
		t.Fatalf("redis: want exactly 1 proxy deployed, got %d: %+v", len(eng2.deployed), eng2.deployed)
	}
}

// reconcileProxy's disable path (ExternalPort == nil) removes the proxy
// service and must tolerate a not-found ServiceRemove error (proxy was never
// deployed, or already removed) while still surfacing a real docker error.
func TestReconcileProxyDisabledToleratesNotFoundOnServiceRemove(t *testing.T) {
	eng := newMockEngine()
	eng.removeErr = errors.New("Error: no such service: krill-dbproxy-1")
	svc := newSvc(eng, newFakeStore(Instance{}))
	inst := samplePGInstance()
	inst.ExternalPort = nil
	if err := svc.reconcileProxy(context.Background(), inst); err != nil {
		t.Fatalf("not-found ServiceRemove error must be tolerated, got: %v", err)
	}
	if len(eng.removed) != 1 || eng.removed[0] != proxyName(inst.ID) {
		t.Errorf("removed = %+v, want [%s]", eng.removed, proxyName(inst.ID))
	}
}

func TestReconcileProxyDisabledSurfacesRealServiceRemoveError(t *testing.T) {
	eng := newMockEngine()
	eng.removeErr = errors.New("cannot connect to the Docker daemon")
	svc := newSvc(eng, newFakeStore(Instance{}))
	inst := samplePGInstance()
	inst.ExternalPort = nil
	err := svc.reconcileProxy(context.Background(), inst)
	if err == nil {
		t.Fatal("want error when ServiceRemove fails with a non-not-found error")
	}
	if len(eng.removed) != 1 || eng.removed[0] != proxyName(inst.ID) {
		t.Errorf("removed = %+v, want [%s]", eng.removed, proxyName(inst.ID))
	}
}

// reconcileProxy's enable path (a target with a non-nil HostPort) must
// propagate a ServiceDeploy failure rather than swallow it — this is the
// deploy-side counterpart of TestReconcileProxyDisabledSurfacesRealServiceRemoveError.
func TestReconcileProxySurfacesServiceDeployError(t *testing.T) {
	eng := newMockEngine()
	eng.deployErr = errors.New("cannot connect to the Docker daemon")
	svc := newSvc(eng, newFakeStore(Instance{}))
	inst := samplePGInstance()
	inst.ExternalPort = p32(5433)
	err := svc.reconcileProxy(context.Background(), inst)
	if err == nil {
		t.Fatal("want error when ServiceDeploy fails")
	}
	if len(eng.deployed) != 1 {
		t.Errorf("deployed = %+v, want exactly 1 attempted deploy", eng.deployed)
	}
}
