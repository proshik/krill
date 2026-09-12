package dbservice

import (
	"context"
	"errors"
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

// A stopped instance is moved onto its organization network without being
// started: the service is rewritten at the replica count it was given (zero for
// a stopped one), the proxy follows it, nothing is pulled and the recorded
// status is left alone. Skipping it instead would leave it on the shared
// network, where a later Start — which only scales the service — would bring it
// back up.
func TestParkInstanceMovesAStoppedInstanceWithoutStartingIt(t *testing.T) {
	inst := samplePGInstance()
	inst.ExternalPort = p32(5433)
	eng := newMockEngine()
	st := newFakeStore(inst)
	st.orgNetwork = "krill-org-2"
	st.status[inst.ID] = "idle"
	svc := newSvc(eng, st)

	if err := svc.ParkInstance(context.Background(), inst.ID, 0); err != nil {
		t.Fatalf("ParkInstance: %v", err)
	}
	if len(eng.deployed) != 2 {
		t.Fatalf("want 2 deploys (instance + proxy), got %d: %+v", len(eng.deployed), eng.deployed)
	}
	if got := eng.deployed[0]; got.Name != inst.AppName || got.Replicas != 0 || got.Network != "krill-org-2" {
		t.Fatalf("instance spec = name %q replicas %d network %q, want %q/0/krill-org-2", got.Name, got.Replicas, got.Network, inst.AppName)
	}
	if got := eng.deployed[1].Network; got != "krill-org-2" {
		t.Fatalf("proxy network = %q, want krill-org-2", got)
	}
	for _, ref := range eng.pulled {
		if ref == inst.Image {
			t.Fatalf("parking must not pull the instance image, pulled %v", eng.pulled)
		}
	}
	if got := st.st(inst.ID); got != "idle" {
		t.Fatalf("status = %q, parking must leave it alone", got)
	}
}

// A deploy failure is reported, so the migration leaves the organization
// unmarked instead of flagging a stopped instance moved when it was not.
func TestParkInstanceReportsADeployFailure(t *testing.T) {
	eng := newMockEngine()
	eng.deployErr = errors.New("boom")
	svc := newSvc(eng, newFakeStore(samplePGInstance()))
	if err := svc.ParkInstance(context.Background(), 1, 0); err == nil {
		t.Fatal("a failed service deploy must be reported")
	}
}
