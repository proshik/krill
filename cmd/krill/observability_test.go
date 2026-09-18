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
// labelled or not — to know how many nodes the agent is expected to reach on
// this pass (see observability.Coverage); only the rendered krill_node
// mapping skips an unnamed node.
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
		{Hostname: "node-1.example.com", Name: "control-plane", Ready: true},
		{Hostname: "worker-host", Name: "worker-1", Ready: true},
		{Hostname: "unlabelled-host", Name: "", Ready: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("obsNodeNames = %+v, want %+v", got, want)
	}
}

// TestObsNodeNamesKeepsUnlabelledNodesForCoverage proves a node without a
// node_labels row is still returned (empty Name, so its krill_node rule is
// still skipped — see RenderNodeConfig), rather than dropped from the list
// entirely: dropping it would make Reconcile undercount how many nodes the
// agent is expected to reach.
func TestObsNodeNamesKeepsUnlabelledNodesForCoverage(t *testing.T) {
	swarmNodes := []docker.SwarmNode{
		{ID: "mgr-1", Hostname: "host-a", Role: "manager", Leader: true, State: "ready", Availability: "active"},
		{ID: "wrk-1", Hostname: "host-b", Role: "worker", State: "down", Availability: "active"},
	}
	got := obsNodeNames(swarmNodes, nil)
	want := []observability.NodeName{
		{Hostname: "host-a", Name: "", Ready: true},
		{Hostname: "host-b", Name: "", Ready: false}, // down: not expected this pass
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("obsNodeNames with no labels = %+v, want %+v", got, want)
	}
}

// TestObsNodeNamesReadyRequiresReadyAndActive covers the exact Coverage
// threshold: State must be "ready" AND Availability must be "active" — any
// other combination (down, drained, paused) is not ready.
func TestObsNodeNamesReadyRequiresReadyAndActive(t *testing.T) {
	swarmNodes := []docker.SwarmNode{
		{ID: "a", Hostname: "ready-active", State: "ready", Availability: "active"},
		{ID: "b", Hostname: "ready-drain", State: "ready", Availability: "drain"},
		{ID: "c", Hostname: "down-active", State: "down", Availability: "active"},
		{ID: "d", Hostname: "unknown-pause", State: "unknown", Availability: "pause"},
	}
	got := obsNodeNames(swarmNodes, nil)
	wantReady := map[string]bool{"ready-active": true, "ready-drain": false, "down-active": false, "unknown-pause": false}
	for _, n := range got {
		if n.Ready != wantReady[n.Hostname] {
			t.Errorf("node %s: Ready = %v, want %v", n.Hostname, n.Ready, wantReady[n.Hostname])
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
