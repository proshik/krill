// Package topology builds the read-only cluster topology graph (nodes, the
// services/DBs placed on them, and app->DB links) from neutral inputs. It is
// pure: no database, docker, or i18n dependencies, so the graph logic —
// grouping, cross-node detection, and dangling-link pruning — is unit-tested
// in isolation. The caller (internal/server) adapts DB rows and Docker tasks
// into Inputs and marshals the returned Graph to JSON verbatim.
package topology

import (
	"sort"
	"strconv"
	"strings"
)

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
	Kind   string `json:"kind"`   // "app" | "db" | "gateway" | "internet"
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
	From      string `json:"from"`       // source GService.ID
	ToKind    string `json:"to_kind"`    // "logical" | "instance" | "service"
	ToID      string `json:"to_id"`      // logical db id string | service GService.ID ("db-<id>"/"app-<id>"/"gateway")
	Kind      string `json:"kind"`       // "db" | "ingress"
	Detected  bool   `json:"detected"`   // db kind: env-detected vs modeled app_db_link
	Label     string `json:"label"`      // domain (ingress) | aggregated vars (collapsed db)
	VarName   string `json:"var"`        // env var name (single db link tooltip)
	Field     string `json:"field"`      // url|password|host|port|user|dbname
	Engine    string `json:"engine"`     // "postgres" | "redis" (thread color)
	CrossNode bool   `json:"cross_node"` // db kind only: app and target on different nodes
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
	From     string
	ToKind   string // "logical" | "instance" | "service"
	ToID     string
	Kind     string // "db" (default) | "ingress"
	Detected bool
	Label    string
	VarName  string
	Field    string
	Engine   string
}

// EnvInstance is a DB instance to scan env values for, by its overlay hostname.
type EnvInstance struct {
	ServiceID string // "db-<instanceID>"
	AppName   string // db_instances.app_name = overlay DNS hostname
	Engine    string // "postgres" | "redis"
}

// EnvLogical pinpoints a postgres connection to a logical-DB chip.
type EnvLogical struct {
	ID              int64
	InstanceAppName string // owning instance's app_name
	DbName          string
}

// DetectedLink is a connection inferred from a raw env value.
type DetectedLink struct {
	ToKind  string // "logical" | "instance"
	ToID    string // logical db id string | "db-<instanceID>"
	Engine  string
	VarName string // the env var whose value matched
}

// dsnHasDB reports whether val contains a "/<dbname>" path segment terminated by
// end-of-string or a non-identifier char (so "/readeck" does not match dbname "read").
func dsnHasDB(val, dbname string) bool {
	needle := "/" + dbname
	for i := 0; ; {
		j := strings.Index(val[i:], needle)
		if j < 0 {
			return false
		}
		end := i + j + len(needle)
		if end == len(val) || !isDBNameChar(val[end]) {
			return true
		}
		i += j + 1
	}
}

func isDBNameChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// DetectEnvLinks scans env values for each instance's overlay hostname
// (<app_name>:5432 for postgres, :6379 for redis). A postgres match whose value
// also contains "/<db_name>" for a logical DB in that instance targets the
// logical DB (chip); otherwise the instance. At most one detection per instance;
// env keys are scanned in sorted order for determinism.
func DetectEnvLinks(env map[string]string, instances []EnvInstance, logicals []EnvLogical) []DetectedLink {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []DetectedLink
	for _, inst := range instances {
		port := ":5432"
		if inst.Engine == "redis" {
			port = ":6379"
		}
		token := inst.AppName + port
		matchedVar, matchedVal := "", ""
		for _, k := range keys {
			if strings.Contains(env[k], token) {
				matchedVar, matchedVal = k, env[k]
				break
			}
		}
		if matchedVar == "" {
			continue
		}
		if inst.Engine == "postgres" {
			logicalID := ""
			for _, lg := range logicals {
				if lg.InstanceAppName == inst.AppName && dsnHasDB(matchedVal, lg.DbName) {
					logicalID = strconv.FormatInt(lg.ID, 10)
					break
				}
			}
			if logicalID != "" {
				out = append(out, DetectedLink{ToKind: "logical", ToID: logicalID, Engine: "postgres", VarName: matchedVar})
				continue
			}
		}
		out = append(out, DetectedLink{ToKind: "instance", ToID: inst.ServiceID, Engine: inst.Engine, VarName: matchedVar})
	}
	return out
}

// MergeLinks collapses db links sharing (From,ToKind,ToID) into one: multiple
// modeled vars aggregate into Label; a detected link is dropped when a modeled
// link already covers the same edge; a detected-only edge is kept as detected.
// It is intended for db links (modeled + detected) — ingress links have unique
// edges and should bypass it. First-appearance order is preserved.
func MergeLinks(links []LinkInput) []LinkInput {
	type group struct {
		idx     int
		modeled bool
		vars    []string
	}
	seen := map[string]*group{}
	var out []LinkInput
	for _, l := range links {
		key := l.From + "|" + l.ToKind + "|" + l.ToID
		g := seen[key]
		if g == nil {
			out = append(out, l)
			ng := &group{idx: len(out) - 1, modeled: !l.Detected}
			if !l.Detected && l.VarName != "" {
				ng.vars = append(ng.vars, l.VarName)
			}
			seen[key] = ng
			continue
		}
		if !l.Detected {
			// defensive: adapters append modeled before detected, so this rarely runs
			if !g.modeled { // replace a detected placeholder with the modeled link
				out[g.idx] = l
				g.modeled = true
				g.vars = nil
			}
			if l.VarName != "" {
				g.vars = append(g.vars, l.VarName)
			}
		}
		// a detected duplicate of an existing edge is dropped
	}
	for _, g := range seen {
		if g.modeled && len(g.vars) > 1 {
			out[g.idx].Label = strings.Join(g.vars, ", ")
			out[g.idx].VarName = ""
		}
	}
	return out
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
			continue // source not in this org's set
		}
		var toNode string
		switch l.ToKind {
		case "logical":
			id, err := strconv.ParseInt(l.ToID, 10, 64)
			if err != nil || !dbExists[id] {
				continue // dangling logical db
			}
			toNode = dbNode[id]
		case "instance", "service":
			n, ok := svcNode[l.ToID]
			if !ok {
				continue // dangling target service
			}
			toNode = n
		default:
			continue
		}
		kind := l.Kind
		if kind == "" {
			kind = "db"
		}
		cross := kind == "db" && fromNode != "" && toNode != "" && fromNode != toNode
		g.Links = append(g.Links, GLink{
			From: l.From, ToKind: l.ToKind, ToID: l.ToID, Kind: kind, Detected: l.Detected,
			Label: l.Label, VarName: l.VarName, Field: l.Field, Engine: l.Engine, CrossNode: cross,
		})
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
