package dbservice

import (
	"context"
	"errors"
	"testing"
)

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

// reconcileProxy's disable path (ExternalPort == nil) removes the proxy
// service and must tolerate a not-found ServiceRemove error (proxy was never
// deployed, or already removed) while still surfacing a real docker error.
func TestReconcileProxyDisabledToleratesNotFoundOnServiceRemove(t *testing.T) {
	eng := newMockEngine()
	eng.removeErr = errors.New("Error: no such service: krill-dbproxy-1")
	svc := newSvc(eng, nil)
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
	svc := newSvc(eng, nil)
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
