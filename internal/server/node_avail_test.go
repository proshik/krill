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
		if got := nodeAvailByHostname(live, c.host); got != c.want {
			t.Errorf("byHostname(%q)=%v want %v", c.host, got, c.want)
		}
		if got := nodeAvailByID(live, c.host); got != c.want {
			t.Errorf("byID(%q)=%v want %v", c.host, got, c.want)
		}
	}
}

func TestDisplayInstanceStatus(t *testing.T) {
	live := nodes([2]string{"w1", "ready"}, [2]string{"w2", "down"})
	if displayInstanceStatus(true, "gone", live, "running") != "running" {
		t.Error("running service should always be running")
	}
	if displayInstanceStatus(false, "gone", live, "running") != "node_removed" {
		t.Error("pinned to removed node -> node_removed")
	}
	if displayInstanceStatus(false, "w2", live, "running") != "node_down" {
		t.Error("pinned to down node -> node_down")
	}
	if displayInstanceStatus(false, "", live, "error") != "error" {
		t.Error("control-plane, not running -> stored status")
	}
	if displayInstanceStatus(false, "w1", live, "idle") != "idle" {
		t.Error("live node, not running -> stored status")
	}
}

func TestDisplayAppStatus(t *testing.T) {
	live := nodes([2]string{"w1", "ready"}, [2]string{"w2", "down"})
	if displayAppStatus("running", "pin", "gone", live) != "running" {
		t.Error("running app untouched")
	}
	if displayAppStatus("deploying", "any", "", live) != "deploying" {
		t.Error("non-pin app untouched")
	}
	if displayAppStatus("deploying", "pin", "gone1,gone2", live) != "node_removed" {
		t.Error("all placement nodes removed -> node_removed")
	}
	if displayAppStatus("deploying", "pin", "w2,gone", live) != "node_down" {
		t.Error("some down, none live -> node_down")
	}
	if displayAppStatus("deploying", "pin", "w1,gone", live) != "deploying" {
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
