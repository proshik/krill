package main

import (
	"reflect"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/observability"
)

// TestObsNodeNamesUsesNodeLabels pins the source of the krill_node mapping:
// it comes from node_labels — the same display name the Nodes page,
// topology and app page already show — not cluster_nodes (which holds only
// workers) and not a hardcoded name for the manager. A node with no label
// still comes back (Name ""), because Reconcile needs the FULL node list —
// labelled or not — to know how many nodes it still expects a container on
// (see observability.Coverage); only the rendered krill_node mapping skips
// an unnamed node.
func TestObsNodeNamesUsesNodeLabels(t *testing.T) {
	swarmNodes := []docker.SwarmNode{
		{ID: "mgr-1", Hostname: "node-1.example.com", Role: "manager", Leader: true, State: "ready", Availability: "active"},
		{ID: "wrk-1", Hostname: "worker-host", Role: "worker", State: "ready", Availability: "active"},
		{ID: "wrk-2", Hostname: "unlabelled-host", Role: "worker", State: "ready", Availability: "active"},
	}
	labels := []db.NodeLabel{
		{SwarmNodeID: "mgr-1", Label: "control-plane"},
		{SwarmNodeID: "wrk-1", Label: "worker-1"},
		// wrk-2 has no row at all — never labelled.
	}
	got := obsNodeNames(swarmNodes, labels)
	want := []observability.NodeName{
		{Hostname: "node-1.example.com", Name: "control-plane", Expected: true},
		{Hostname: "worker-host", Name: "worker-1", Expected: true},
		{Hostname: "unlabelled-host", Name: "", Expected: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("obsNodeNames = %+v, want %+v", got, want)
	}
}

// TestObsNodeNamesKeepsUnlabelledNodesForCoverage proves a node without a
// node_labels row is still returned (empty Name, so its krill_node rule is
// still skipped — see RenderNodeConfig), rather than dropped from the list
// entirely: dropping it would make Reconcile undercount how many nodes it
// still expects a container on. A DOWN node is still Expected: Swarm leaves
// its last container running there until it is actually drained.
func TestObsNodeNamesKeepsUnlabelledNodesForCoverage(t *testing.T) {
	swarmNodes := []docker.SwarmNode{
		{ID: "mgr-1", Hostname: "host-a", Role: "manager", Leader: true, State: "ready", Availability: "active"},
		{ID: "wrk-1", Hostname: "host-b", Role: "worker", State: "down", Availability: "active"},
	}
	got := obsNodeNames(swarmNodes, nil)
	want := []observability.NodeName{
		{Hostname: "host-a", Name: "", Expected: true},
		{Hostname: "host-b", Name: "", Expected: true}, // down but not drained: still expected
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("obsNodeNames with no labels = %+v, want %+v", got, want)
	}
}

// TestObsNodeNamesExpectedIgnoresStateOnlyAvailabilityDrain covers the exact
// Coverage threshold (the live-acceptance fix): Expected depends only on
// Availability != "drain" — State plays no part. A DOWN or an unresponsive
// ("unknown" State) node is still Expected, because that is precisely the
// case Coverage exists to catch: Swarm cannot reach it to update its task,
// but the container it last placed there keeps running. Only draining
// actively removes the task, which is why it is the sole exclusion.
func TestObsNodeNamesExpectedIgnoresStateOnlyAvailabilityDrain(t *testing.T) {
	swarmNodes := []docker.SwarmNode{
		{ID: "a", Hostname: "ready-active", State: "ready", Availability: "active"},
		{ID: "b", Hostname: "ready-drain", State: "ready", Availability: "drain"},
		{ID: "c", Hostname: "down-active", State: "down", Availability: "active"},
		{ID: "d", Hostname: "unknown-pause", State: "unknown", Availability: "pause"},
	}
	got := obsNodeNames(swarmNodes, nil)
	wantExpected := map[string]bool{
		"ready-active":  true,
		"ready-drain":   false, // the only exclusion
		"down-active":   true,  // down, not drained: still expected
		"unknown-pause": true,  // unresponsive, not drained: still expected
	}
	for _, n := range got {
		if n.Expected != wantExpected[n.Hostname] {
			t.Errorf("node %s: Expected = %v, want %v", n.Hostname, n.Expected, wantExpected[n.Hostname])
		}
	}
}

// TestObsNodeNamesEmptyInputs guards the boundary cases: no swarm nodes, or
// a label row for a swarm ID that no longer exists (a removed node whose
// label row lingered, or simply hasn't been cleaned up).
func TestObsNodeNamesEmptyInputs(t *testing.T) {
	if got := obsNodeNames(nil, nil); len(got) != 0 {
		t.Errorf("obsNodeNames(nil, nil) = %+v, want empty", got)
	}
	got := obsNodeNames(nil, []db.NodeLabel{{SwarmNodeID: "gone", Label: "ghost"}})
	if len(got) != 0 {
		t.Errorf("obsNodeNames with a label for a node not in the swarm list = %+v, want empty", got)
	}
}
