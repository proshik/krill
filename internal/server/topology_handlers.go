package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/topology"
	"github.com/proshik/krill/internal/traefik"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// topology renders the org-scoped cluster topology page. The graph itself is
// fetched client-side from topologyData.
func (s *Server) topology(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.Topology(o, role))
}

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
	graph, err := s.buildTopology(ctx, o.ID, live, tasks, s.nodeLabelMap(ctx))
	if err != nil {
		logFrom(r).Error("topology: build failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	body, err := json.Marshal(graph)
	if err != nil {
		logFrom(r).Error("topology: encode failed", "err", err)
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// buildTopology gathers the org's resources, resolves each service's node from
// live tasks, and returns the assembled graph (with the ingress lane, exposed-
// domain edges, and env-detected DB links).
func (s *Server) buildTopology(ctx context.Context, orgID int64, live []docker.SwarmNode, tasks []docker.TaskInfo, labels map[string]string) (topology.Graph, error) {
	// ---- cluster node lanes ----
	leaderID := ""
	nodeByHost := map[string]string{}
	nodeName := map[string]string{} // node id -> display name
	liveID := make(map[string]bool, len(live))
	nodes := make([]topology.NodeInput, 0, len(live)+2)
	for _, n := range live {
		name := nodeDisplayName(n, labels)
		nodeByHost[n.Hostname] = n.ID
		nodeName[n.ID] = name
		liveID[n.ID] = true
		if (n.Leader || n.Role == "manager") && leaderID == "" {
			leaderID = n.ID
		}
		nodes = append(nodes, topology.NodeInput{ID: n.ID, Name: name, Role: n.Role, State: n.State, Leader: n.Leader})
	}

	// ---- service -> running node ids ----
	tref := make([]topology.Task, 0, len(tasks))
	for _, t := range tasks {
		tref = append(tref, topology.Task{Service: t.ServiceName, Node: t.NodeID, Running: t.State == "running"})
	}
	running := topology.ResolveNodes(tref)

	var (
		services     []topology.ServiceInput
		dbLinks      []topology.LinkInput // modeled + env-detected app->DB
		ingressLinks []topology.LinkInput // Internet->Traefik->app
		unplaced     bool
	)

	// ---- ingress lane: Internet + Traefik gateway ----
	nodes = append(nodes, topology.NodeInput{ID: "ingress", Name: i18n.T(ctx, "topo.ingress")})
	services = append(services, topology.ServiceInput{ID: "internet", Kind: "internet", NodeID: "ingress", Label: i18n.T(ctx, "topo.internet")})
	gwSub, gwStatus := "", "stopped"
	if tn := running[traefik.ServiceName]; len(tn) > 0 {
		gwStatus, gwSub = deploy.StatusRunning, nodeName[tn[0]]
	}
	services = append(services, topology.ServiceInput{ID: "gateway", Kind: "gateway", NodeID: "ingress", Label: i18n.T(ctx, "topo.gateway"), Sub: gwSub, Status: gwStatus})
	ingressLinks = append(ingressLinks, topology.LinkInput{From: "internet", ToKind: "service", ToID: "gateway", Kind: "ingress"})

	// ---- apps: org -> projects -> envs -> apps ----
	projects, err := s.q.ListProjectsWithCounts(ctx, orgID)
	if err != nil {
		return topology.Graph{}, fmt.Errorf("topology: list projects failed: %w", err)
	}
	type envInfo struct{ Env, Proj string }
	envMeta := map[int64]envInfo{}
	var envIDs []int64
	for _, p := range projects {
		envs, err := s.q.ListEnvironments(ctx, p.ID)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list environments failed: %w", err)
		}
		for _, e := range envs {
			envMeta[e.ID] = envInfo{Env: e.Name, Proj: p.Name}
			envIDs = append(envIDs, e.ID)
		}
	}
	var apps []db.Application
	if len(envIDs) > 0 {
		apps, err = s.q.ListApplicationsByEnvironmentIDs(ctx, envIDs)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list applications failed: %w", err)
		}
	}
	for _, a := range apps {
		sid := "app-" + strconv.FormatInt(a.ID, 10)
		svcName := docker.ServiceName(a.ID)
		node := ""
		if got := running[svcName]; len(got) > 0 {
			node = got[0]
		} else if a.PlacementMode == "pin" && a.PlacementNodes != "" {
			if id := firstCSV(a.PlacementNodes); liveID[id] {
				node = id
			}
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
		// modeled app->DB links
		modeled, err := s.q.ListDBLinksByApplication(ctx, a.ID)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list db links failed: %w", err)
		}
		for _, l := range modeled {
			li := topology.LinkInput{From: sid, Kind: "db", VarName: l.VarName, Field: l.Field}
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
			dbLinks = append(dbLinks, li)
		}
		// ingress edges: Traefik -> app per exposed domain
		doms, err := s.q.ListDomainsByApplication(ctx, a.ID)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list domains failed: %w", err)
		}
		for _, d := range doms {
			if !d.Exposed {
				continue
			}
			ingressLinks = append(ingressLinks, topology.LinkInput{From: "gateway", ToKind: "service", ToID: sid, Kind: "ingress", Label: d.Host})
		}
	}

	// ---- DB instances ----
	insts, err := s.q.ListDBInstancesByOrg(ctx, orgID)
	if err != nil {
		return topology.Graph{}, fmt.Errorf("topology: list db instances failed: %w", err)
	}
	instEngine := map[int64]string{}
	var envInstances []topology.EnvInstance
	for _, in := range insts {
		instEngine[in.ID] = in.Engine
		sid := "db-" + strconv.FormatInt(in.ID, 10)
		node := ""
		if got := running[in.AppName]; len(got) > 0 {
			node = got[0]
		} else if in.NodeHostname != "" {
			node = nodeByHost[in.NodeHostname]
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
		envInstances = append(envInstances, topology.EnvInstance{ServiceID: sid, AppName: in.AppName, Engine: in.Engine})
	}

	// ---- logical databases (chips) ----
	instNode := map[string]string{}
	for _, sv := range services {
		if sv.Kind == "db" {
			instNode[sv.ID] = sv.NodeID
		}
	}
	var dbs []topology.DBInput
	var envLogicals []topology.EnvLogical
	for _, eid := range envIDs {
		lds, err := s.q.ListLogicalDatabasesByEnvironment(ctx, eid)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list logical databases failed: %w", err)
		}
		for _, ld := range lds {
			isid := "db-" + strconv.FormatInt(ld.InstanceID, 10)
			dbs = append(dbs, topology.DBInput{ID: ld.ID, ServiceID: isid, Name: ld.Name, Env: envMeta[eid].Env, NodeID: instNode[isid]})
			envLogicals = append(envLogicals, topology.EnvLogical{ID: ld.ID, InstanceAppName: ld.InstanceAppName, DbName: ld.DbName})
		}
	}

	// ---- env-detected app->DB connections (raw connection strings) ----
	for _, a := range apps {
		env, _ := parseEnv(a.EnvText)
		sid := "app-" + strconv.FormatInt(a.ID, 10)
		for _, d := range topology.DetectEnvLinks(env, envInstances, envLogicals) {
			dbLinks = append(dbLinks, topology.LinkInput{
				From: sid, ToKind: d.ToKind, ToID: d.ToID, Kind: "db", Detected: true, Engine: d.Engine, VarName: d.VarName,
			})
		}
	}

	// instance-links carry the target instance's real engine (defensive)
	for i := range dbLinks {
		if dbLinks[i].ToKind != "instance" {
			continue
		}
		idStr := strings.TrimPrefix(dbLinks[i].ToID, "db-")
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
			if eng := instEngine[id]; eng != "" {
				dbLinks[i].Engine = eng
			}
		}
	}

	if unplaced {
		nodes = append(nodes, topology.NodeInput{ID: "unplaced", Name: i18n.T(ctx, "topo.unplaced")})
	}

	allLinks := append(topology.MergeLinks(dbLinks), ingressLinks...)
	return topology.Build(topology.Inputs{Nodes: nodes, Services: services, Dbs: dbs, Links: allLinks}), nil
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
