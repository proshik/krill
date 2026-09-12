package dbservice

import (
	"context"
	"io"
	"testing"
)

// TestDeployedInstanceUsesTheOrganizationNetwork is the database-side
// counterpart of internal/deploy's TestSpecUsesTheOrganizationNetwork: a DB
// instance's own service AND its external-access proxy must deploy into its
// organization's overlay network, not the single shared fallback — and an
// organization that hasn't been migrated yet (network_name == "") must still
// deploy, via the deployer's configured fallback network.
//
// This goes through deployCore — the same path DeployInstance (and therefore
// production) uses — rather than calling instanceSpec/proxySpecTarget
// directly, so a regression that drops the networkFor wiring from deployCore
// or reconcileProxy (while leaving networkFor itself intact) is still caught.
func TestDeployedInstanceUsesTheOrganizationNetwork(t *testing.T) {
	inst := samplePGInstance()
	inst.ExternalPort = p32(5433) // gives the postgres driver an external target, so reconcileProxy deploys a proxy instead of just removing one

	// Organization migrated: both the instance and its proxy land on the org network.
	eng := newMockEngine()
	st := newFakeStore(inst)
	st.orgNetwork = "krill-org-2"
	svc := newSvc(eng, st)

	if err := svc.deployCore(context.Background(), inst.ID, io.Discard); err != nil {
		t.Fatalf("deployCore: %v", err)
	}
	if len(eng.deployed) != 2 {
		t.Fatalf("want 2 deploys (instance + proxy), got %d: %+v", len(eng.deployed), eng.deployed)
	}
	if got := eng.deployed[0].Network; got != "krill-org-2" {
		t.Fatalf("instance network = %q, want krill-org-2", got)
	}
	if got := eng.deployed[1].Network; got != "krill-org-2" {
		t.Fatalf("proxy network = %q, want krill-org-2", got)
	}

	// Organization not migrated yet (orgNetwork == ""): both fall back to the
	// deployer's configured network ("krill-net", set by newSvc).
	eng2 := newMockEngine()
	st2 := newFakeStore(inst)
	svc2 := newSvc(eng2, st2)

	if err := svc2.deployCore(context.Background(), inst.ID, io.Discard); err != nil {
		t.Fatalf("deployCore (fallback): %v", err)
	}
	if len(eng2.deployed) != 2 {
		t.Fatalf("want 2 deploys (instance + proxy), got %d: %+v", len(eng2.deployed), eng2.deployed)
	}
	if got := eng2.deployed[0].Network; got != "krill-net" {
		t.Fatalf("fallback instance network = %q, want krill-net", got)
	}
	if got := eng2.deployed[1].Network; got != "krill-net" {
		t.Fatalf("fallback proxy network = %q, want krill-net", got)
	}
}
