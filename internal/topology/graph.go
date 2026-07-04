// Package topology builds the read-only cluster topology graph (nodes, the
// services/DBs placed on them, and app->DB links) from neutral inputs. It is
// pure: no database, docker, or i18n dependencies, so the graph logic —
// grouping, cross-node detection, and dangling-link pruning — is unit-tested
// in isolation. The caller (internal/server) adapts DB rows and Docker tasks
// into Inputs and marshals the returned Graph to JSON verbatim.
package topology

import "strconv"

// ---- JSON output (the wire contract; json tags are the single source) ----

type GNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	State  string `json:"state"`
	Leader bool   `json:"leader"`
}

type GService struct {
	ID     string `json:"id"`     // "app-<appID>" | "db-<instanceID>"
	Kind   string `json:"kind"`   // "app" | "db"
	NodeID string `json:"node"`   // FK -> GNode.ID
	Label  string `json:"label"`  // display name
	Sub    string `json:"sub"`    // "project / env" (app) or "Postgres"/"Redis" (db)
	Engine string `json:"engine"` // "" | "postgres" | "redis"
	Status string `json:"status"` // "running" | stored status (greys the box when not running)
}

type GDB struct {
	ID        int64  `json:"id"`      // logical database id
	ServiceID string `json:"service"` // owning instance's GService.ID ("db-<instanceID>")
	Name      string `json:"name"`
	Env       string `json:"env"`
}

type GLink struct {
	From      string `json:"from"`       // app GService.ID ("app-<appID>")
	ToKind    string `json:"to_kind"`    // "logical" | "instance"
	ToID      string `json:"to_id"`      // logical db id string | "db-<instanceID>"
	VarName   string `json:"var"`        // env var name (tooltip)
	Field     string `json:"field"`      // url|password|host|port|user|dbname
	Engine    string `json:"engine"`     // "postgres" | "redis" (thread color)
	CrossNode bool   `json:"cross_node"` // app and target on different nodes
}

type Graph struct {
	Nodes    []GNode    `json:"nodes"`
	Services []GService `json:"services"`
	Dbs      []GDB      `json:"dbs"`
	Links    []GLink    `json:"links"`
}

// ---- Neutral inputs (built by the caller; node ids already resolved) ----

type NodeInput struct {
	ID     string
	Name   string
	Role   string
	State  string
	Leader bool
}

type ServiceInput struct {
	ID     string
	Kind   string
	NodeID string // resolved node id, or "" if unplaced
	Label  string
	Sub    string
	Engine string
	Status string
}

type DBInput struct {
	ID        int64
	ServiceID string // owning instance GService.ID
	Name      string
	Env       string
	NodeID    string // owning instance's node id (for cross-node calc)
}

type LinkInput struct {
	From    string
	ToKind  string // "logical" | "instance"
	ToID    string // logical db id string | "db-<instanceID>"
	VarName string
	Field   string
	Engine  string
}

// Task is one running-or-not task used by ResolveNodes.
type Task struct {
	Service string
	Node    string
	Running bool
}

// ResolveNodes maps each service name to its distinct running node ids
// (first-seen order). Non-running tasks and tasks with an empty service/node
// are ignored, so a service with no running task is absent from the result.
func ResolveNodes(tasks []Task) map[string][]string {
	out := map[string][]string{}
	seen := map[string]map[string]bool{}
	for _, t := range tasks {
		if !t.Running || t.Service == "" || t.Node == "" {
			continue
		}
		if seen[t.Service] == nil {
			seen[t.Service] = map[string]bool{}
		}
		if seen[t.Service][t.Node] {
			continue
		}
		seen[t.Service][t.Node] = true
		out[t.Service] = append(out[t.Service], t.Node)
	}
	return out
}

// Build assembles the graph. Nodes, services and logical DBs pass through
// unchanged; each link's target node is resolved via the service/DB sets to
// compute CrossNode, and links whose source or target is missing are dropped
// (so the client never draws a thread to nothing).
func Build(in Inputs) Graph {
	g := Graph{
		Nodes:    make([]GNode, 0, len(in.Nodes)),
		Services: make([]GService, 0, len(in.Services)),
		Dbs:      make([]GDB, 0, len(in.Dbs)),
		Links:    make([]GLink, 0, len(in.Links)),
	}
	for _, n := range in.Nodes {
		g.Nodes = append(g.Nodes, GNode{ID: n.ID, Name: n.Name, Role: n.Role, State: n.State, Leader: n.Leader})
	}
	svcNode := make(map[string]string, len(in.Services))
	for _, s := range in.Services {
		g.Services = append(g.Services, GService{ID: s.ID, Kind: s.Kind, NodeID: s.NodeID, Label: s.Label, Sub: s.Sub, Engine: s.Engine, Status: s.Status})
		svcNode[s.ID] = s.NodeID
	}
	dbNode := make(map[int64]string, len(in.Dbs))
	dbExists := make(map[int64]bool, len(in.Dbs))
	for _, d := range in.Dbs {
		g.Dbs = append(g.Dbs, GDB{ID: d.ID, ServiceID: d.ServiceID, Name: d.Name, Env: d.Env})
		dbNode[d.ID] = d.NodeID
		dbExists[d.ID] = true
	}
	for _, l := range in.Links {
		fromNode, ok := svcNode[l.From]
		if !ok {
			continue // source app not in this org's set
		}
		var toNode string
		switch l.ToKind {
		case "logical":
			id, err := strconv.ParseInt(l.ToID, 10, 64)
			if err != nil || !dbExists[id] {
				continue // dangling logical db
			}
			toNode = dbNode[id]
		case "instance":
			n, ok := svcNode[l.ToID]
			if !ok {
				continue // dangling instance
			}
			toNode = n
		default:
			continue
		}
		cross := fromNode != "" && toNode != "" && fromNode != toNode
		g.Links = append(g.Links, GLink{From: l.From, ToKind: l.ToKind, ToID: l.ToID, VarName: l.VarName, Field: l.Field, Engine: l.Engine, CrossNode: cross})
	}
	return g
}

// Inputs is the full neutral input to Build.
type Inputs struct {
	Nodes    []NodeInput
	Services []ServiceInput
	Dbs      []DBInput
	Links    []LinkInput
}
