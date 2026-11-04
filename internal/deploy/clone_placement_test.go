package deploy

import (
	"context"
	"errors"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

type placementEngine struct {
	docker.Engine
	labels map[string]map[string]string
	fail   bool
}

func (e *placementEngine) Nodes(context.Context) ([]docker.SwarmNode, error) {
	return []docker.SwarmNode{{ID: "owned-node"}, {ID: "extra-node"}}, nil
}
func (e *placementEngine) NodeSetLabel(_ context.Context, node, key, value string) error {
	if e.fail {
		return errors.New("node unavailable")
	}
	e.labels[node][key] = value
	return nil
}
func (e *placementEngine) NodeDeleteLabel(_ context.Context, node, key string) error {
	delete(e.labels[node], key)
	return nil
}
func TestDeployMaterializesOwnPlacementWithoutChangingSource(t *testing.T) {
	e := &placementEngine{labels: map[string]map[string]string{"owned-node": {"krill.place.100": "1"}, "extra-node": {"krill.place.100": "1", "krill.place.101": "1"}}}
	d := &Deployer{engine: e}
	app := App{ID: 101, PlacementMode: "pin", PlacementNodes: []string{"owned-node"}}
	if err := d.ensurePlacement(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	if e.labels["owned-node"]["krill.place.100"] != "1" || e.labels["owned-node"]["krill.place.101"] != "1" {
		t.Fatal("placement identity not isolated")
	}
	if e.labels["extra-node"]["krill.place.101"] != "" || e.labels["extra-node"]["krill.place.100"] != "1" {
		t.Fatal("extra eligibility or source label changed")
	}
	e.fail = true
	if err := d.ensurePlacement(t.Context(), app); err == nil {
		t.Fatal("placement failure silently ignored")
	}
	app.PlacementNodes = []string{"missing-node"}
	if err := d.ensurePlacement(t.Context(), app); err == nil {
		t.Fatal("missing node silently ignored")
	}
}
