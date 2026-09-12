package deploy

import (
	"testing"

	"github.com/proshik/krill/internal/traefik"
)

// TestSpecUsesTheOrganizationNetwork is the regression guard for the tenant
// network boundary: an app must deploy into its organization's overlay
// network (App.Network), not the single shared network every organization
// used to share. An app whose organization has not been migrated yet
// (Network == "") must still deploy, falling back to the deployer's
// configured network.
//
// The App used here carries an exposed domain: AppLabels short-circuits to
// just {"traefik.enable": "false"} (no "traefik.docker.network" label at all)
// when no domain is exposed, which would make the network-label assertion
// meaningless.
func TestSpecUsesTheOrganizationNetwork(t *testing.T) {
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))

	app := App{
		ID: 1, Name: "app", Image: "nginx", Tag: "alpine", Port: 80,
		Domains: []traefik.Domain{{Host: "app.example", TLS: false, Exposed: true}},
		Network: "krill-org-2",
	}
	spec := d.buildSpec(app, "i")
	if spec.Network != "krill-org-2" {
		t.Fatalf("service network = %q, want krill-org-2", spec.Network)
	}
	if got := spec.Labels["traefik.docker.network"]; got != "krill-org-2" {
		t.Fatalf("traefik network label = %q, want krill-org-2", got)
	}

	// An app whose organization has not been migrated yet still deploys.
	app.Network = ""
	spec = d.buildSpec(app, "i")
	if spec.Network != "krill-net" {
		t.Fatalf("fallback network = %q, want krill-net", spec.Network)
	}
}
