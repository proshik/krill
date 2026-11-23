package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/proshik/krill/internal/auth"
	"log/slog"
	"net/http"
	"sort"
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

// topology renders the org-scoped cluster topology: an ingress lane and one
// lane per node with the organization's apps and servers placed on them;
// the client draws the links between the cards. ?project= narrows it to one
// project's apps and the servers they use.
func (s *Server) topology(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	graph, ok := s.topologyGraph(w, r, o.ID)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.Topology(o, role, topoView(r.Context(), o.ID, graph, r.URL.Query().Get("project"))))
}

// topologyData serves the org-scoped cluster topology as JSON (nodes, the org's
// apps/DB instances placed on them, logical DBs, and app->DB links). Org-admin
// gated; every query is scoped to the active org.
func (s *Server) topologyData(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	graph, ok := s.topologyGraph(w, r, o.ID)
	if !ok {
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

// topologyGraph reads the cluster and builds the organization's graph. A
// failed node or task listing degrades to empty lanes (logged); a failed
// query of Krill's own data is an error.
func (s *Server) topologyGraph(w http.ResponseWriter, r *http.Request, orgID int64) (topology.Graph, bool) {
	ctx := r.Context()
	var live []docker.SwarmNode
	var tasks []docker.TaskInfo
	if s.engine != nil {
		var err error
		if live, err = s.engine.Nodes(ctx); err != nil {
			logFrom(r).Warn("topology: list nodes failed, lanes left empty", "err", err)
		}
		if tasks, err = s.engine.Tasks(ctx); err != nil {
			logFrom(r).Warn("topology: list tasks failed, placement falls back to stored nodes", "err", err)
		}
	}
	graph, err := s.buildTopology(ctx, orgID, live, tasks, s.nodeLabelMap(ctx))
	if err != nil {
		logFrom(r).Error("topology: build failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return topology.Graph{}, false
	}
	return graph, true
}

// topoView turns the graph into lanes of cards. Every link becomes a target
// in the card's data-to, which is what the map draws; the ones found only in
// env values are also listed in data-detected, which styles them. A logical
// database's link ends at its chip inside the server's card, so an app shows
// which database it uses. Several links (one per injected field) can reach
// one target: each target is listed once, and it counts as detected only
// when no modeled link reaches it. project is a project ID ("" for all).
func topoView(ctx context.Context, orgID int64, g topology.Graph, project string) templates.TopoView {
	elem := func(serviceID string) string { return "t-" + serviceID }
	dbOwner := map[string]string{}
	holds := map[string][]templates.TopoHold{}
	for _, d := range g.Dbs {
		id := strconv.FormatInt(d.ID, 10)
		dbOwner[id] = d.ServiceID
		holds[d.ServiceID] = append(holds[d.ServiceID], templates.TopoHold{ElemID: elem("ldb-" + id), Name: d.Name, Env: d.Env})
	}
	to := map[string][]string{}
	detected := map[string][]string{}
	modeled := map[[2]string]bool{}  // from, target: reached by a modeled link
	listed := map[[2]string]bool{}   // from, target: already in data-to
	found := map[[2]string]bool{}    // from, target: reached by a detected link
	ingress := map[string][]string{} // app service -> domains
	servedProjects := map[string]map[string]bool{}
	projectOf := map[string]string{} // app service -> project ID
	projectName := map[string]string{}
	for _, sv := range g.Services {
		if sv.Kind == "app" && sv.ProjectID != 0 {
			id := strconv.FormatInt(sv.ProjectID, 10)
			projectOf[sv.ID], projectName[id] = id, sv.Project
		}
	}
	for _, l := range g.Links {
		target, owner := l.ToID, l.ToID
		if l.ToKind == "logical" {
			target, owner = "ldb-"+l.ToID, dbOwner[l.ToID]
		}
		if owner == "" {
			continue
		}
		pair := [2]string{l.From, target}
		if !listed[pair] {
			listed[pair] = true
			to[l.From] = append(to[l.From], elem(target))
		}
		switch {
		case l.Kind == "ingress":
			modeled[pair] = true
			if l.Label != "" {
				ingress[target] = append(ingress[target], l.Label)
			}
		case l.Detected:
			found[pair] = true
		default:
			modeled[pair] = true
		}
		if p := projectOf[l.From]; p != "" {
			if servedProjects[owner] == nil {
				servedProjects[owner] = map[string]bool{}
			}
			servedProjects[owner][p] = true
		}
	}
	for pair := range found {
		if !modeled[pair] {
			detected[pair[0]] = append(detected[pair[0]], elem(pair[1]))
		}
	}
	for from := range detected {
		sort.Strings(detected[from])
	}
	visible := func(sv topology.GService) bool {
		if project == "" {
			return true
		}
		switch sv.Kind {
		case "app":
			return projectOf[sv.ID] == project
		case "db":
			return servedProjects[sv.ID][project]
		}
		return true
	}
	v := templates.TopoView{Project: project}
	for id, name := range projectName {
		v.Projects = append(v.Projects, templates.TopoProject{ID: id, Name: name})
	}
	sort.Slice(v.Projects, func(i, j int) bool {
		if v.Projects[i].Name != v.Projects[j].Name {
			return v.Projects[i].Name < v.Projects[j].Name
		}
		return v.Projects[i].ID < v.Projects[j].ID
	})
	// Lanes are matched to services by node ID: display names can repeat.
	byNode := map[string]*templates.TopoLane{}
	for _, n := range g.Nodes {
		lane := templates.TopoLane{ID: n.ID, Name: n.Name}
		if n.ID == "ingress" {
			lane.Ingress = true
		} else if n.ID != "unplaced" {
			if auth.IsInstanceAdmin(ctx) { // the Nodes page is theirs only
				lane.Href = "/orgs/" + strconv.FormatInt(orgID, 10) + "/nodes#node-" + n.Name
			}
			if n.State == "ready" {
				lane.StateKind, lane.StateText = "ok", i18n.T(ctx, "nodes.state_ready")
			} else {
				lane.StateKind, lane.StateText = "err", i18n.T(ctx, "nodes.state_down")
			}
		}
		v.Lanes = append(v.Lanes, lane)
	}
	// The ingress lane goes first.
	sort.SliceStable(v.Lanes, func(i, j int) bool { return v.Lanes[i].Ingress && !v.Lanes[j].Ingress })
	for i := range v.Lanes {
		byNode[v.Lanes[i].ID] = &v.Lanes[i]
	}
	base := "/orgs/" + strconv.FormatInt(orgID, 10)
	for _, sv := range g.Services {
		lane := byNode[sv.NodeID]
		if lane == nil || !visible(sv) {
			continue
		}
		c := templates.TopoCard{ElemID: elem(sv.ID), Name: sv.Label, Sub: sv.Sub, To: to[sv.ID], Detected: detected[sv.ID]}
		switch sv.Kind {
		case "internet":
			c.Glyph, c.System, c.To = "i-link", true, to[sv.ID]
			lane.Apps = append(lane.Apps, c)
			continue
		case "gateway":
			c.Glyph, c.Gateway = "i-node", true
			c.StateKind, c.StateText = templates.ServerState(ctx, sv.Status)
			if project != "" {
				var keep []string
				for _, t := range c.To {
					if projectOf[strings.TrimPrefix(t, "t-")] == project {
						keep = append(keep, t)
					}
				}
				c.To = keep
			}
			lane.Apps = append(lane.Apps, c)
			continue
		case "app":
			c.Glyph, c.Projects = "i-app", projectOf[sv.ID]
			c.StateKind, c.StateText = templates.AppState(ctx, sv.Status)
			c.Line = strings.Join(ingress[sv.ID], ", ")
			c.Href = sv.Href
			lane.Apps = append(lane.Apps, c)
		case "db":
			c.Glyph = engineGlyph(sv.Engine)
			c.StateKind, c.StateText = templates.ServerState(ctx, sv.Status)
			c.Href = base + "/db-servers/" + strings.TrimPrefix(sv.ID, "db-")
			c.Holds = holds[sv.ID]
			lane.Servers = append(lane.Servers, c)
		}
	}
	return v
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
	type envInfo struct {
		Env, Proj string
		ProjID    int64
	}
	envMeta := map[int64]envInfo{}
	var envIDs []int64
	// One query for every project's environments, not one per project: this view
	// renders the whole org, so the per-project loop was a query per project on
	// a page that is already fanning out to docker.
	projName := make(map[int64]string, len(projects))
	projIDs := make([]int64, 0, len(projects))
	for _, p := range projects {
		projName[p.ID] = p.Name
		projIDs = append(projIDs, p.ID)
	}
	if len(projIDs) > 0 {
		envs, err := s.q.ListEnvironmentsByProjectIDs(ctx, projIDs)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list environments failed: %w", err)
		}
		for _, e := range envs {
			envMeta[e.ID] = envInfo{Env: e.Name, Proj: projName[e.ProjectID], ProjID: e.ProjectID}
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
	// Links and domains for every app in one query each (previously two queries
	// per application).
	appIDs := make([]int64, 0, len(apps))
	for _, a := range apps {
		appIDs = append(appIDs, a.ID)
	}
	linksByApp := map[int64][]db.ListDBLinksByApplicationIDsRow{}
	domsByApp := map[int64][]db.Domain{}
	if len(appIDs) > 0 {
		rows, err := s.q.ListDBLinksByApplicationIDs(ctx, appIDs)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list db links failed: %w", err)
		}
		for _, l := range rows {
			linksByApp[l.ApplicationID] = append(linksByApp[l.ApplicationID], l)
		}
		drows, err := s.q.ListDomainsByApplicationIDs(ctx, appIDs)
		if err != nil {
			return topology.Graph{}, fmt.Errorf("topology: list domains failed: %w", err)
		}
		for _, d := range drows {
			domsByApp[d.ApplicationID] = append(domsByApp[d.ApplicationID], d)
		}
	}
	// An app with no running task is either stopped or was never deployed;
	// only Swarm can tell them apart (see deploy.DisplayStatus).
	var idle []string
	for _, a := range apps {
		if svc := docker.ServiceName(a.ID); len(running[svc]) == 0 {
			idle = append(idle, svc)
		}
	}
	var idleStates map[string]docker.ServiceState
	if s.engine != nil && len(idle) > 0 {
		if st, err := s.engine.ServiceStates(ctx, idle); err != nil {
			slog.Warn("topology: service states", "err", err)
		} else {
			idleStates = st
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
		} else if idleStates != nil {
			status = deploy.DisplayStatus(idleStates[svcName], a.Status)
		}
		m := envMeta[a.EnvironmentID]
		services = append(services, topology.ServiceInput{
			ID: sid, Kind: "app", NodeID: node, Label: a.Name, Sub: m.Proj + " / " + m.Env, Status: status,
			Href:      envURL(orgID, m.ProjID, a.EnvironmentID) + "/apps/" + strconv.FormatInt(a.ID, 10),
			ProjectID: m.ProjID, Project: m.Proj,
		})
		// modeled app->DB links
		for _, l := range linksByApp[a.ID] {
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
		var hosts []string
		for _, d := range domsByApp[a.ID] {
			// A redirecting domain never reaches the app: it bounces to the primary.
			if d.Exposed && !d.RedirectToPrimary {
				if paths := traefik.NormalizePaths(traefik.SplitPaths(d.Paths)); len(paths) > 0 {
					for _, p := range paths {
						hosts = append(hosts, d.Host+p)
					}
				} else {
					hosts = append(hosts, d.Host)
				}
			}
		}
		if len(hosts) > 0 {
			ingressLinks = append(ingressLinks, topology.LinkInput{From: "gateway", ToKind: "service", ToID: sid, Kind: "ingress", Label: strings.Join(hosts, ", ")})
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
	case "valkey":
		return "Valkey"
	case "dragonfly":
		return "DragonFly"
	case "minio":
		return "MinIO"
	default:
		return e
	}
}
