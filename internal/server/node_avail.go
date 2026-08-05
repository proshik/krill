package server

import (
	"strings"

	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
)

// NodeAvail classifies a pinned node against the live swarm node list.
type NodeAvail string

const (
	NodeLive    NodeAvail = "live"
	NodeDown    NodeAvail = "down"
	NodeRemoved NodeAvail = "removed"
	// NodeUnknown: the live node list could not be read, so nothing can be said
	// about this node. Distinct from NodeRemoved on purpose — a failed
	// engine.Nodes() call returns an empty list, and treating that as "removed"
	// paints healthy, pinned infrastructure with an alarming badge.
	NodeUnknown NodeAvail = "unknown"
)

// nodeAvailByHostname classifies a node referenced by hostname (DB instances).
// "" => NodeLive (control-plane / not pinned). listOK reports whether live was
// actually read; when false the answer is NodeUnknown.
func nodeAvailByHostname(live []docker.SwarmNode, listOK bool, hostname string) NodeAvail {
	if hostname == "" {
		return NodeLive
	}
	if !listOK {
		return NodeUnknown
	}
	for _, n := range live {
		if n.Hostname == hostname {
			if n.State == "down" {
				return NodeDown
			}
			return NodeLive
		}
	}
	return NodeRemoved
}

// nodeAvailByID classifies a node referenced by swarm node ID (app placement).
func nodeAvailByID(live []docker.SwarmNode, listOK bool, id string) NodeAvail {
	if id == "" {
		return NodeLive
	}
	if !listOK {
		return NodeUnknown
	}
	for _, n := range live {
		if n.ID == id {
			if n.State == "down" {
				return NodeDown
			}
			return NodeLive
		}
	}
	return NodeRemoved
}

// displayInstanceStatus is the honest badge for a DB instance: running when the
// service converged; node_down/node_removed when its pinned node is unavailable;
// otherwise the stored status.
func displayInstanceStatus(running bool, nodeHostname string, live []docker.SwarmNode, listOK bool, stored string) string {
	if running {
		return "running"
	}
	switch nodeAvailByHostname(live, listOK, nodeHostname) {
	case NodeRemoved:
		return "node_removed"
	case NodeDown:
		return "node_down"
	}
	return stored
}

// displayAppStatus overrides a pinned app's derived status with a node badge
// when none of its placement nodes are live (so it can never schedule).
func displayAppStatus(derived, placementMode, placementNodes string, live []docker.SwarmNode, listOK bool) string {
	if derived == deploy.StatusRunning || placementMode != "pin" || placementNodes == "" {
		return derived
	}
	if !listOK {
		return derived // nothing can be said about the pins right now
	}
	anyLive, anyDown := false, false
	for _, id := range splitPlacement(placementNodes) {
		switch nodeAvailByID(live, listOK, id) {
		case NodeLive:
			anyLive = true
		case NodeDown:
			anyDown = true
		}
	}
	if anyLive {
		return derived
	}
	if anyDown {
		return "node_down"
	}
	return "node_removed"
}

// filterLiveNodes keeps only submitted node IDs present in the live cluster.
func filterLiveNodes(submitted []string, live []docker.SwarmNode) (kept []string, dropped int) {
	liveSet := map[string]bool{}
	for _, n := range live {
		liveSet[n.ID] = true
	}
	for _, id := range submitted {
		if liveSet[id] {
			kept = append(kept, id)
		} else {
			dropped++
		}
	}
	return kept, dropped
}

// placementContainsNode reports whether a comma-separated placement_nodes list
// includes swarmID.
func placementContainsNode(placementNodes, swarmID string) bool {
	for _, id := range splitPlacement(placementNodes) {
		if id == swarmID {
			return true
		}
	}
	return false
}

func splitPlacement(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
