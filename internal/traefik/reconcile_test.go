package traefik

import (
	"context"
	"errors"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

// fakeEngine records what Reconcile asks of Docker and plays back the labels of
// the service it last deployed, so a second Reconcile sees a running gateway.
// Every other Engine method panics (embedded nil interface) — Reconcile must
// not reach for anything else.
type fakeEngine struct {
	docker.Engine
	ensured  []string
	deployed []docker.ServiceSpec
	labels   map[string]string
	found    bool
	labelErr error
}

func (f *fakeEngine) NetworkEnsure(_ context.Context, name string) error {
	f.ensured = append(f.ensured, name)
	return nil
}

func (f *fakeEngine) ServiceDeploy(_ context.Context, spec docker.ServiceSpec) error {
	f.deployed = append(f.deployed, spec)
	f.labels = spec.Labels
	f.found = true
	return nil
}

func (f *fakeEngine) ServiceLabels(_ context.Context, _ string) (map[string]string, bool, error) {
	if f.labelErr != nil {
		return nil, false, f.labelErr
	}
	return f.labels, f.found, nil
}

func TestReconcileAttachesBaseAndOrgNetworks(t *testing.T) {
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, acme); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := []string{"krill-net", "krill-org-1", "krill-org-2"}
	if len(f.ensured) != len(want) {
		t.Fatalf("ensured = %v, want %v", f.ensured, want)
	}
	for i, n := range want {
		if f.ensured[i] != n {
			t.Fatalf("ensured = %v, want %v", f.ensured, want)
		}
	}
	if len(f.deployed) != 1 {
		t.Fatalf("want one deploy, got %d", len(f.deployed))
	}
	got := f.deployed[0].Networks
	if len(got) != len(want) {
		t.Fatalf("spec networks = %v, want %v", got, want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Fatalf("spec networks = %v, want %v (base first, stable order)", got, want)
		}
	}
	// The router default network is the base one: a router with no explicit
	// network label must still resolve.
	if !hasArg(f.deployed[0].Args, "--providers.swarm.network=krill-net") {
		t.Fatalf("swarm provider network arg missing: %v", f.deployed[0].Args)
	}
}

// Swarm recreates the Traefik task on every ServiceDeploy (ForceUpdate is
// bumped unconditionally), so a reconcile that changes nothing must not deploy
// at all — otherwise every control-plane restart and every unrelated call would
// blip ingress.
func TestReconcileIsIdempotent(t *testing.T) {
	f := &fakeEngine{}
	acme := AcmeConfig{Email: "a@b.c"}
	orgs := []string{"krill-org-1"}
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := Reconcile(context.Background(), f, "krill-net", orgs, acme); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("unchanged reconcile redeployed: %d deploys", len(f.deployed))
	}

	// A new organization changes the network set, so the gateway has to move.
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, acme); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if len(f.deployed) != 2 {
		t.Fatalf("a changed network set must redeploy: %d deploys", len(f.deployed))
	}

	// So does a changed Let's Encrypt configuration: the fingerprint covers the
	// whole spec, not only the networks.
	if err := Reconcile(context.Background(), f, "krill-net", []string{"krill-org-1", "krill-org-2"}, AcmeConfig{Email: "a@b.c", Staging: true}); err != nil {
		t.Fatalf("fourth reconcile: %v", err)
	}
	if len(f.deployed) != 3 {
		t.Fatalf("a changed acme config must redeploy: %d deploys", len(f.deployed))
	}
}

// If the current labels cannot be read we cannot tell whether anything changed;
// deploying is the safe answer (a redundant restart beats a gateway that never
// learns about a new network).
func TestReconcileDeploysWhenLabelsUnreadable(t *testing.T) {
	f := &fakeEngine{labelErr: errors.New("boom")}
	if err := Reconcile(context.Background(), f, "krill-net", nil, AcmeConfig{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deployed) != 1 {
		t.Fatalf("want a deploy despite the read failure, got %d", len(f.deployed))
	}
}
