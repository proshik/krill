package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/topology"
	"github.com/proshik/krill/internal/web/i18n"
)

// topologyData serves the org-scoped cluster topology as JSON (nodes, the org's
// apps/DB instances placed on them, logical DBs, and app->DB links). Org-admin
// gated; every query is scoped to the active org.
func (s *Server) topologyData(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	live, _ := s.engine.Nodes(ctx) // degrade to empty lanes on error
	tasks, _ := s.engine.Tasks(ctx)
	graph := s.buildTopology(ctx, o.ID, live, tasks, s.nodeLabelMap(ctx))

	body, err := json.Marshal(graph)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// buildTopology gathers the org's resources, resolves each service's node from
// live tasks (falling back to placement pins / control-plane), and returns the
// assembled graph.
func (s *Server) buildTopology(ctx context.Context, orgID int64, live []docker.SwarmNode, tasks []docker.TaskInfo, labels map[string]string) topology.Graph {
	// ---- nodes (lanes) ----
	leaderID := ""
	nodeByHost := map[string]string{}
	nodes := make([]topology.NodeInput, 0, len(live))
	for _, n := range live {
		nodeByHost[n.Hostname] = n.ID
		if (n.Leader || n.Role == "manager") && leaderID == "" {
			leaderID = n.ID
		}
		nodes = append(nodes, topology.NodeInput{
			ID: n.ID, Name: nodeDisplayName(n, labels), Role: n.Role, State: n.State, Leader: n.Leader,
		})
	}

	// ---- service -> running node ids (from live tasks) ----
	tref := make([]topology.Task, 0, len(tasks))
	for _, t := range tasks {
		tref = append(tref, topology.Task{Service: t.ServiceName, Node: t.NodeID, Running: t.State == "running"})
	}
	running := topology.ResolveNodes(tref)

	var (
		services []topology.ServiceInput
		links    []topology.LinkInput
		unplaced bool
	)

	// ---- apps: org -> projects -> envs -> apps ----
	projects, _ := s.q.ListProjectsWithCounts(ctx, orgID)
	type envInfo struct{ Env, Proj string }
	envMeta := map[int64]envInfo{}
	var envIDs []int64
	for _, p := range projects {
		envs, _ := s.q.ListEnvironments(ctx, p.ID)
		for _, e := range envs {
			envMeta[e.ID] = envInfo{Env: e.Name, Proj: p.Name}
			envIDs = append(envIDs, e.ID)
		}
	}
	var apps []db.Application
	if len(envIDs) > 0 {
		apps, _ = s.q.ListApplicationsByEnvironmentIDs(ctx, envIDs)
	}
	for _, a := range apps {
		sid := "app-" + strconv.FormatInt(a.ID, 10)
		svcName := docker.ServiceName(a.ID)
		node := ""
		if got := running[svcName]; len(got) > 0 {
			node = got[0]
		} else if a.PlacementMode == "pin" && a.PlacementNodes != "" {
			node = firstCSV(a.PlacementNodes)
		} else {
			node = leaderID
		}
		if node == "" {
			unplaced = true
			node = "unplaced"
		}
		status := a.Status
		if len(running[svcName]) > 0 {
			status = deploy.StatusRunning
		}
		m := envMeta[a.EnvironmentID]
		services = append(services, topology.ServiceInput{
			ID: sid, Kind: "app", NodeID: node, Label: a.Name, Sub: m.Proj + " / " + m.Env, Status: status,
		})
		dblinks, _ := s.q.ListDBLinksByApplication(ctx, a.ID)
		for _, l := range dblinks {
			li := topology.LinkInput{From: sid, VarName: l.VarName, Field: l.Field}
			switch {
			case l.LogicalDatabaseID != nil:
				li.ToKind = "logical"
				li.ToID = strconv.FormatInt(*l.LogicalDatabaseID, 10)
				li.Engine = "postgres"
			case l.InstanceID != nil:
				li.ToKind = "instance"
				li.ToID = "db-" + strconv.FormatInt(*l.InstanceID, 10)
				li.Engine = "redis"
			default:
				continue
			}
			links = append(links, li)
		}
	}

	// ---- DB instances ----
	insts, _ := s.q.ListDBInstancesByOrg(ctx, orgID)
	instEngine := map[int64]string{}
	for _, in := range insts {
		instEngine[in.ID] = in.Engine
		sid := "db-" + strconv.FormatInt(in.ID, 10)
		node := ""
		if got := running[in.AppName]; len(got) > 0 {
			node = got[0]
		} else if in.NodeHostname != "" {
			node = nodeByHost[in.NodeHostname] // "" if that node is gone
		} else {
			node = leaderID
		}
		if node == "" {
			unplaced = true
			node = "unplaced"
		}
		status := in.Status
		if len(running[in.AppName]) > 0 {
			status = deploy.StatusRunning
		}
		services = append(services, topology.ServiceInput{
			ID: sid, Kind: "db", NodeID: node, Label: in.Name, Sub: titleEngine(in.Engine), Engine: in.Engine, Status: status,
		})
	}

	// ---- logical databases (chips inside their instance) ----
	instNode := map[string]string{}
	for _, sv := range services {
		if sv.Kind == "db" {
			instNode[sv.ID] = sv.NodeID
		}
	}
	var dbs []topology.DBInput
	for _, eid := range envIDs {
		lds, _ := s.q.ListLogicalDatabasesByEnvironment(ctx, eid)
		for _, ld := range lds {
			isid := "db-" + strconv.FormatInt(ld.InstanceID, 10)
			dbs = append(dbs, topology.DBInput{
				ID: ld.ID, ServiceID: isid, Name: ld.Name, Env: envMeta[eid].Env, NodeID: instNode[isid],
			})
		}
	}

	// Instance-links carry the target instance's real engine (redis-only today,
	// but resolve defensively from the instance set).
	for i := range links {
		if links[i].ToKind != "instance" {
			continue
		}
		idStr := strings.TrimPrefix(links[i].ToID, "db-")
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
			if eng := instEngine[id]; eng != "" {
				links[i].Engine = eng
			}
		}
	}

	if unplaced {
		nodes = append(nodes, topology.NodeInput{ID: "unplaced", Name: i18n.T(ctx, "topo.unplaced")})
	}

	return topology.Build(topology.Inputs{Nodes: nodes, Services: services, Dbs: dbs, Links: links})
}

// nodeDisplayName matches the Monitoring/DB-servers convention: the manager is
// always "control-plane"; workers use a custom label if set, else the hostname.
func nodeDisplayName(n docker.SwarmNode, labels map[string]string) string {
	if n.Leader || n.Role == "manager" {
		return "control-plane"
	}
	if l := labels[n.ID]; l != "" {
		return l
	}
	return n.Hostname
}

func firstCSV(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		return s[:i]
	}
	return s
}

func titleEngine(e string) string {
	switch e {
	case "postgres":
		return "Postgres"
	case "redis":
		return "Redis"
	default:
		return e
	}
}
