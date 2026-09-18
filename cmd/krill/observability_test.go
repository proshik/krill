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
// workers) and not a hardcoded name for the manager.
func TestObsNodeNamesUsesNodeLabels(t *testing.T) {
	swarmNodes := []docker.SwarmNode{
		{ID: "mgr-1", Hostname: "node-1.example.com", Role: "manager", Leader: true},
		{ID: "wrk-1", Hostname: "worker-host", Role: "worker"},
		{ID: "wrk-2", Hostname: "unlabelled-host", Role: "worker"},
	}
	labels := []db.NodeLabel{
		{SwarmNodeID: "mgr-1", Label: "control-plane"},
		{SwarmNodeID: "wrk-1", Label: "worker-1"},
		// wrk-2 has no row at all — never labelled.
	}
	got := obsNodeNames(swarmNodes, labels)
	want := []observability.NodeName{
		{Hostname: "node-1.example.com", Name: "control-plane"},
		{Hostname: "worker-host", Name: "worker-1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("obsNodeNames = %+v, want %+v", got, want)
	}
}

// TestObsNodeNamesSkipsUnlabelledNodes proves a node without a node_labels
// row is left out of the mapping entirely, rather than falling back to some
// other name — an install that has never labelled any node (the normal case
// right after upgrading to this feature) gets no mapping at all, and every
// node's krill_node keeps showing its raw Swarm hostname exactly as before.
func TestObsNodeNamesSkipsUnlabelledNodes(t *testing.T) {
	swarmNodes := []docker.SwarmNode{
		{ID: "mgr-1", Hostname: "host-a", Role: "manager", Leader: true},
		{ID: "wrk-1", Hostname: "host-b", Role: "worker"},
	}
	if got := obsNodeNames(swarmNodes, nil); len(got) != 0 {
		t.Errorf("obsNodeNames with no labels = %+v, want empty", got)
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
