package server

import (
	"reflect"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

func nodes(spec ...[2]string) []docker.SwarmNode { // {id, state} with hostname=id
	var out []docker.SwarmNode
	for _, s := range spec {
		out = append(out, docker.SwarmNode{ID: s[0], Hostname: s[0], State: s[1]})
	}
	return out
}

func TestNodeAvail(t *testing.T) {
	live := nodes([2]string{"a", "ready"}, [2]string{"b", "down"})
	cases := []struct {
		host string
		want NodeAvail
	}{{"", NodeLive}, {"a", NodeLive}, {"b", NodeDown}, {"gone", NodeRemoved}}
	for _, c := range cases {
		if got := nodeAvailByHostname(live, true, c.host); got != c.want {
			t.Errorf("byHostname(%q)=%v want %v", c.host, got, c.want)
		}
		if got := nodeAvailByID(live, true, c.host); got != c.want {
			t.Errorf("byID(%q)=%v want %v", c.host, got, c.want)
		}
	}
}

func TestDisplayInstanceStatus(t *testing.T) {
	live := nodes([2]string{"w1", "ready"}, [2]string{"w2", "down"})
	if displayInstanceStatus(true, "gone", live, true, "running") != "running" {
		t.Error("running service should always be running")
	}
	if displayInstanceStatus(false, "gone", live, true, "running") != "node_removed" {
		t.Error("pinned to removed node -> node_removed")
	}
	if displayInstanceStatus(false, "w2", live, true, "running") != "node_down" {
		t.Error("pinned to down node -> node_down")
	}
	if displayInstanceStatus(false, "", live, true, "error") != "error" {
		t.Error("control-plane, not running -> stored status")
	}
	if displayInstanceStatus(false, "w1", live, true, "idle") != "idle" {
		t.Error("live node, not running -> stored status")
	}
}

func TestDisplayAppStatus(t *testing.T) {
	live := nodes([2]string{"w1", "ready"}, [2]string{"w2", "down"})
	if displayAppStatus("running", "pin", "gone", live, true) != "running" {
		t.Error("running app untouched")
	}
	if displayAppStatus("deploying", "any", "", live, true) != "deploying" {
		t.Error("non-pin app untouched")
	}
	if displayAppStatus("deploying", "pin", "gone1,gone2", live, true) != "node_removed" {
		t.Error("all placement nodes removed -> node_removed")
	}
	if displayAppStatus("deploying", "pin", "w2,gone", live, true) != "node_down" {
		t.Error("some down, none live -> node_down")
	}
	if displayAppStatus("deploying", "pin", "w1,gone", live, true) != "deploying" {
		t.Error("at least one live node -> keep derived")
	}
}

func TestFilterLiveNodes(t *testing.T) {
	live := nodes([2]string{"w1", "ready"}, [2]string{"w2", "ready"})
	kept, dropped := filterLiveNodes([]string{"w1", "gone", "w2"}, live)
	if !reflect.DeepEqual(kept, []string{"w1", "w2"}) || dropped != 1 {
		t.Fatalf("kept=%v dropped=%d", kept, dropped)
	}
}

func TestPlacementContainsNode(t *testing.T) {
	if !placementContainsNode("a,b,c", "b") {
		t.Error("should contain b")
	}
	if placementContainsNode("a,b", "x") {
		t.Error("should not contain x")
	}
}

// A failed engine.Nodes() call yields an empty list, which used to be
// indistinguishable from "the cluster has no such node". Every pinned resource
// then rendered as node_removed — an alarming badge produced by a transient
// API hiccup, on infrastructure that is perfectly healthy.
func TestNodeAvailUnknownWhenListUnavailable(t *testing.T) {
	if got := nodeAvailByHostname(nil, false, "w1"); got != NodeUnknown {
		t.Errorf("nodeAvailByHostname with an unreadable list = %q, want %q", got, NodeUnknown)
	}
	if got := nodeAvailByID(nil, false, "abc123"); got != NodeUnknown {
		t.Errorf("nodeAvailByID with an unreadable list = %q, want %q", got, NodeUnknown)
	}
	// A readable-but-empty list still means removed: the node is genuinely gone.
	if got := nodeAvailByHostname(nil, true, "w1"); got != NodeRemoved {
		t.Errorf("nodeAvailByHostname with a readable empty list = %q, want %q", got, NodeRemoved)
	}
}

// With the node list unreadable the badge must fall back to the stored status
// rather than claim the node was removed.
func TestDisplayStatusFallsBackWhenNodeListUnknown(t *testing.T) {
	if got := displayInstanceStatus(false, "w1", nil, false, "idle"); got != "idle" {
		t.Errorf("displayInstanceStatus = %q, want the stored %q", got, "idle")
	}
	if got := displayAppStatus("deploying", "pin", "abc123", nil, false); got != "deploying" {
		t.Errorf("displayAppStatus = %q, want the derived %q", got, "deploying")
	}
	// A readable list keeps the honest badge.
	if got := displayInstanceStatus(false, "w1", nil, true, "idle"); got != "node_removed" {
		t.Errorf("displayInstanceStatus with a readable list = %q, want node_removed", got)
	}
}
