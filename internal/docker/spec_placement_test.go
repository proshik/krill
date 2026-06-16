package docker

import "testing"

func TestBuildSwarmSpecGlobal(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{Name: "x", Image: "i", Global: true})
	if sw.Mode.Global == nil || sw.Mode.Replicated != nil {
		t.Fatalf("global mode not set: %+v", sw.Mode)
	}
}

func TestBuildSwarmSpecReplicatedByDefault(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{Name: "x", Image: "i", Replicas: 2})
	if sw.Mode.Replicated == nil || sw.Mode.Global != nil {
		t.Fatalf("expected replicated mode: %+v", sw.Mode)
	}
}

func TestBuildSwarmSpecSpread(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{
		Name: "x", Image: "i",
		Constraints: []string{"node.labels.krill.place.7==1"}, SpreadNodeID: true,
	})
	p := sw.TaskTemplate.Placement
	if p == nil {
		t.Fatal("placement nil")
	}
	if len(p.Constraints) != 1 || p.Constraints[0] != "node.labels.krill.place.7==1" {
		t.Fatalf("constraints = %v", p.Constraints)
	}
	if len(p.Preferences) != 1 || p.Preferences[0].Spread == nil || p.Preferences[0].Spread.SpreadDescriptor != "node.id" {
		t.Fatalf("spread preference missing/wrong: %+v", p.Preferences)
	}
}

func TestBuildSwarmSpecNoPlacementWhenEmpty(t *testing.T) {
	sw := buildSwarmSpec(ServiceSpec{Name: "x", Image: "i"})
	if sw.TaskTemplate.Placement != nil {
		t.Fatalf("expected no placement, got %+v", sw.TaskTemplate.Placement)
	}
}
