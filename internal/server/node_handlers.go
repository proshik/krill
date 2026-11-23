package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/proshik/krill/internal/cluster"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/topology"
	"github.com/proshik/krill/internal/traefik"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// findSwarmNode returns the live Swarm node with the given ID, if reachable.
func (s *Server) findSwarmNode(ctx context.Context, id string) (docker.SwarmNode, bool) {
	if s.engine == nil {
		return docker.SwarmNode{}, false
	}
	nodes, err := s.engine.Nodes(ctx)
	if err != nil {
		return docker.SwarmNode{}, false
	}
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return docker.SwarmNode{}, false
}

// listNodes renders the cluster: live Swarm nodes annotated with the Krill-managed
// worker rows (readable by any member).
func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var live []docker.SwarmNode
	var tasks []docker.TaskInfo
	if s.engine != nil {
		var err error
		if live, err = s.engine.Nodes(ctx); err != nil {
			logFrom(r).Warn("listNodes: list nodes", "err", err)
		}
		if tasks, err = s.engine.Tasks(ctx); err != nil {
			logFrom(r).Warn("listNodes: list tasks", "err", err)
		}
	}
	rows, err := s.q.ListClusterNodes(ctx)
	if err != nil {
		logFrom(r).Error("listNodes: list cluster nodes failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	lanes := s.nodeLanes(r, o, live, tasks, rows)
	newNode := r.URL.Query().Get("new") == "node" && isAdminRole(role)
	render(w, r, http.StatusOK, templates.Nodes(o, role, lanes, newNode))
}

// nodeLanes puts the organization's apps and servers, and Krill's own
// services, on the nodes their tasks run on; an app's card links to the
// servers it uses.
func (s *Server) nodeLanes(r *http.Request, o db.Organization, live []docker.SwarmNode, tasks []docker.TaskInfo, rows []db.ClusterNode) []templates.NodeLane {
	ctx := r.Context()
	labels := s.nodeLabelMap(ctx)
	managed := map[string]bool{}
	for _, row := range rows {
		if row.SwarmNodeID != "" {
			managed[row.SwarmNodeID] = true
		}
	}
	summary := map[string]monNode{}
	for _, n := range s.nodeSummaries(ctx) {
		summary[n.Node] = n
	}
	ref := make([]topology.Task, 0, len(tasks))
	for _, t := range tasks {
		ref = append(ref, topology.Task{Service: t.ServiceName, Node: t.NodeID, Running: t.State == "running"})
	}
	on := topology.ResolveNodes(ref) // service -> node ids
	ix, err := s.loadOrgIndex(ctx, o.ID)
	if err != nil {
		logFrom(r).Warn("nodeLanes: org index", "err", err)
	}
	s.deriveCardStatuses(ctx, ix.apps, nil)
	insts, err := s.q.ListDBInstancesByOrg(ctx, o.ID)
	if err != nil {
		logFrom(r).Warn("nodeLanes: list servers", "err", err)
	}
	instByID := map[int64]db.DbInstance{}
	for _, in := range insts {
		instByID[in.ID] = in
	}
	// Which servers each app uses.
	uses := map[int64]map[int64]bool{}
	use := func(app, inst int64) {
		if uses[app] == nil {
			uses[app] = map[int64]bool{}
		}
		uses[app][inst] = true
	}
	if ids := ix.appIDs(); len(ids) > 0 {
		if links, err := s.q.ListDBLinksByApplicationIDs(ctx, ids); err == nil {
			ldbInst := map[int64]int64{}
			for envID := range ix.envs {
				if ldbs, err := s.q.ListLogicalDatabasesByEnvironment(ctx, envID); err == nil {
					for _, l := range ldbs {
						ldbInst[l.ID] = l.InstanceID
					}
				}
			}
			for _, l := range links {
				switch {
				case l.InstanceID != nil:
					use(l.ApplicationID, *l.InstanceID)
				case l.LogicalDatabaseID != nil:
					if id, ok := ldbInst[*l.LogicalDatabaseID]; ok {
						use(l.ApplicationID, id)
					}
				}
			}
		}
		if links, err := s.q.ListBucketLinksByApplicationIDs(ctx, ids); err == nil {
			bucketInst := map[int64]int64{}
			for _, in := range insts {
				if in.Engine != "minio" {
					continue
				}
				if bs, err := s.q.ListS3BucketsByInstance(ctx, in.ID); err == nil {
					for _, b := range bs {
						bucketInst[b.ID] = in.ID
					}
				}
			}
			for _, l := range links {
				if id, ok := bucketInst[l.BucketID]; ok {
					use(l.ApplicationID, id)
				}
			}
		}
	}
	metricKeys := s.metricsNodeKeys(ctx, live)
	lanes := make([]templates.NodeLane, 0, len(live))
	idx := map[string]int{}
	for _, n := range live {
		name := nodeDisplayName(n, labels)
		l := templates.NodeLane{Node: n, Name: name, Label: labels[n.ID], Managed: managed[n.ID], CPU: -1, Mem: -1}
		if sm, ok := summary[metricKeys[n.ID]]; ok && !sm.Stale && sm.MemTotal > 0 {
			l.CPU, l.Cores = int(sm.CPUPct+0.5), sm.NCPU
			l.Mem, l.MemUsed, l.MemTotal = int(sm.MemUsed*100/sm.MemTotal), sm.MemUsed, sm.MemTotal
		}
		idx[n.ID] = len(lanes)
		lanes = append(lanes, l)
	}
	sort.SliceStable(lanes, func(i, j int) bool {
		li, lj := lanes[i].Node.Role == "manager", lanes[j].Node.Role == "manager"
		if li != lj {
			return li
		}
		return lanes[i].Name < lanes[j].Name
	})
	for i, l := range lanes {
		idx[l.Node.ID] = i
	}
	base := "/orgs/" + strconv.FormatInt(o.ID, 10)
	for _, a := range ix.apps {
		nodes := on[dockerName(a.ID)]
		var to []string
		var usesNames []string
		for id := range uses[a.ID] {
			if in, ok := instByID[id]; ok {
				to = append(to, "n-inst-"+strconv.FormatInt(id, 10))
				usesNames = append(usesNames, in.Name)
			}
		}
		sort.Strings(to)
		sort.Strings(usesNames)
		kind, text := templates.AppState(ctx, a.Status)
		for i, nid := range nodes {
			li, ok := idx[nid]
			if !ok {
				continue
			}
			sub := ix.envLabel(a.EnvironmentID)
			if len(nodes) > 1 {
				sub = i18n.Tf(ctx, "nodes.replica_of", sub, i+1, len(nodes))
			}
			it := templates.LaneItem{
				ElemID: "n-app-" + strconv.FormatInt(a.ID, 10) + "-" + nid, Name: a.Name, Sub: sub, Href: ix.appURL(a),
				Glyph: "i-app", StateKind: kind, StateText: text, To: to,
			}
			if len(usesNames) > 0 {
				it.Uses = i18n.Tf(ctx, "nodes.uses", strings.Join(usesNames, ", "))
			}
			lanes[li].Apps = append(lanes[li].Apps, it)
		}
	}
	for _, in := range insts {
		nodes := on[in.AppName]
		kind, text := templates.ServerState(ctx, in.Status)
		if len(nodes) > 0 {
			kind, text = templates.ServerState(ctx, "running")
		}
		for _, nid := range nodes {
			li, ok := idx[nid]
			if !ok {
				continue
			}
			lanes[li].Servers = append(lanes[li].Servers, templates.LaneItem{
				ElemID: "n-inst-" + strconv.FormatInt(in.ID, 10), Name: in.Name, Sub: engineLabel(in.Engine),
				Href: base + "/db-servers/" + strconv.FormatInt(in.ID, 10), Glyph: engineGlyph(in.Engine), StateKind: kind, StateText: text,
			})
		}
	}
	// Krill's own services.
	system := map[string]string{
		traefik.ServiceName: i18n.T(ctx, "nodes.sys_gateway"),
		"krill-alloy-node":  i18n.T(ctx, "nodes.sys_agent"),
		"krill-alloy-apps":  i18n.T(ctx, "nodes.sys_collector"),
	}
	names := make([]string, 0, len(system))
	for n := range system {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, svc := range names {
		for _, nid := range on[svc] {
			if li, ok := idx[nid]; ok {
				lanes[li].System = append(lanes[li].System, templates.LaneItem{
					ElemID: "n-sys-" + svc + "-" + nid, Name: svc, Sub: system[svc], Glyph: "i-app",
					StateKind: "ok", StateText: i18n.T(ctx, "state.online"),
				})
			}
		}
	}
	return lanes
}

// nodeLabelMap returns swarm_node_id -> display label. Best-effort: on error it
// returns an empty map so callers fall back to raw hostnames.
func (s *Server) nodeLabelMap(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := s.q.ListNodeLabels(ctx)
	if err != nil {
		return out
	}
	for _, l := range rows {
		out[l.SwarmNodeID] = l.Label
	}
	return out
}

// maxNodeLabelLen bounds the human-readable node display label.
const maxNodeLabelLen = 40

// setNodeLabel sets (or clears) a node's display label by its Swarm ID
// (admin-only). An empty label removes the override; the UI then shows the raw
// hostname.
func (s *Server) setNodeLabel(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	swarmID := chi.URLParam(r, "nodeID")
	label := strings.TrimSpace(r.FormValue("label"))
	if len([]rune(label)) > maxNodeLabelLen {
		s.flashErrT(w, r, "flash.err.node_label_long")
		return
	}
	// This is also the observability agent's krill_node name for this node
	// (see internal/observability.SafeNodeName / cmd/krill's obsNodeNames) —
	// reject the same quote/backslash/control-character set here, at the
	// point the operator actually types the name, instead of only ever
	// finding out from a warning log the next time the agent reconciles.
	// Existing rows saved before this check are unaffected; nodeRules still
	// skips one of those with a warning rather than breaking the config.
	if label != "" && !observability.SafeNodeName(label) {
		s.flashErrT(w, r, "flash.err.node_label_invalid")
		return
	}
	// When the cluster is reachable, only label nodes that actually exist.
	if _, found := s.findSwarmNode(r.Context(), swarmID); s.engine != nil && !found {
		s.flashErrT(w, r, "flash.err.invalid_node")
		return
	}
	if label == "" {
		if err := s.q.DeleteNodeLabel(r.Context(), swarmID); err != nil {
			logFrom(r).Error("setNodeLabel: delete failed", "err", err, "node", swarmID)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
	} else if err := s.q.UpsertNodeLabel(r.Context(), db.UpsertNodeLabelParams{SwarmNodeID: swarmID, Label: label}); err != nil {
		logFrom(r).Error("setNodeLabel: upsert failed", "err", err, "node", swarmID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("node label set", "node", swarmID, "has_label", label != "")
	// The label IS the observability agent's krill_node name for this node
	// (see internal/observability.NodeName / cmd/krill's obsNodeNames), so
	// setting or clearing it changes the agent's rendered config — unlike
	// addNode/removeNode, this is now the actual trigger point (a freshly
	// joined node has no label yet, so joining alone changes nothing there).
	if s.obsWired() {
		s.obs.ctl.Trigger()
	}
	s.flashOK(w, r, "flash.ok.node_label_saved")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/nodes", http.StatusSeeOther)
}

// addNode SSH-joins a worker to the swarm (admin-only). The SSH key and join
// token are never logged; the key is encrypted at rest.
func (s *Server) addNode(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	host := strings.TrimSpace(r.FormValue("ssh_host"))
	user := strings.TrimSpace(r.FormValue("ssh_user"))
	key := r.FormValue("ssh_key")
	if name == "" || host == "" || user == "" || strings.TrimSpace(key) == "" {
		s.flashErrT(w, r, "flash.err.node_fields_required")
		return
	}
	port := 22
	if v := strings.TrimSpace(r.FormValue("ssh_port")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > 65535 {
			s.flashErrT(w, r, "flash.err.invalid_ssh_port")
			return
		}
		port = n
	}
	if s.cfg.AdvertiseAddr == "" {
		s.flashErrT(w, r, "flash.err.advertise_unset")
		return
	}
	if s.engine == nil {
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if n, _ := s.q.CountClusterNodesByName(r.Context(), name); n > 0 {
		s.flashErrT(w, r, "flash.err.node_name_exists")
		return
	}
	token, terr := s.engine.SwarmWorkerToken(r.Context())
	if terr != nil {
		logFrom(r).Error("addNode: get worker token failed", "err", terr)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	// Join over SSH BEFORE persisting, so a failed join leaves no half-added row.
	jctx, jcancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer jcancel()
	_, hostKey, jerr := cluster.Join(jctx, cluster.JoinSpec{
		Host: host, Port: port, User: user, PrivateKey: []byte(key),
		Token: token, ManagerAddr: s.cfg.AdvertiseAddr + ":2377",
	})
	if jerr != nil {
		logFrom(r).Error("addNode: swarm join failed", "err", jerr, "host", host) // never log key/token
		s.flashErrErr(w, r, "flash.err.node_join", jerr)
		return
	}
	row, cerr := s.q.CreateClusterNode(r.Context(), db.CreateClusterNodeParams{
		Name: name, SshHost: host, SshPort: int32(port), SshUser: user,
		SshKey: secret.Enc(key), HostKey: hostKey, SwarmNodeID: "",
	})
	if cerr != nil {
		// The node is already in the swarm but has no row: it would be invisible
		// in the UI and unremovable through it. Undo the join.
		lctx, lcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
		defer lcancel()
		if lerr := cluster.Leave(lctx, cluster.JoinSpec{
			Host: host, Port: port, User: user, PrivateKey: []byte(key), HostKey: hostKey,
		}); lerr != nil {
			logFrom(r).Error("addNode: could not undo the swarm join after a failed row insert — the node is in the swarm with no Krill row; run 'docker swarm leave --force' on it",
				"err", lerr, "host", host)
		} else {
			logFrom(r).Info("addNode: swarm join rolled back after a failed row insert", "host", host)
		}
		logFrom(r).Error("addNode: create node row failed", "err", cerr, "name", name)
		s.flashErrErr(w, r, "flash.err.node_join", cerr)
		return
	}
	// Resolve the new node's Swarm ID by matching its advertised address OR
	// hostname (the operator may have entered either). Warn if unresolved so the
	// blank swarm_node_id (un-removable via UI) is observable, not silent.
	resolved := false
	var swarmAddr net.IP
	if nodes, nerr := s.engine.Nodes(r.Context()); nerr == nil {
		for _, n := range nodes {
			if n.Addr == host || n.Hostname == host {
				swarmAddr = net.ParseIP(n.Addr)
				// A swallowed failure here leaves swarm_node_id empty while
				// claiming the node resolved — the row then looks healthy but is
				// un-removable via the UI and invisible to cross-node exec.
				if serr := s.q.SetClusterNodeSwarmID(r.Context(), db.SetClusterNodeSwarmIDParams{ID: row.ID, SwarmNodeID: n.ID}); serr != nil {
					logFrom(r).Error("addNode: persist swarm id failed", "err", serr, "id", row.ID, "swarm_node_id", n.ID)
				} else {
					resolved = true
				}
				break
			}
		}
	} else {
		logFrom(r).Error("addNode: list swarm nodes failed, cannot resolve swarm id", "err", nerr, "host", host)
	}
	if !resolved {
		logFrom(r).Warn("addNode: joined node not matched to a swarm ID (Addr/hostname mismatch?)", "host", host, "id", row.ID)
	}
	logFrom(r).Info("cluster node added", "name", name, "host", host) // no key/token
	// No observability.Reconciler Trigger here: a freshly joined node has no
	// node_labels row yet (that's a separate, later setNodeLabel call), so
	// the agent's krill_node mapping is unchanged by adding it — it keeps
	// showing the raw hostname until the operator names it.
	if msg := s.joinMTUWarning(r, cluster.JoinSpec{Host: host, Port: port, User: user, PrivateKey: []byte(key), HostKey: hostKey}, name, swarmAddr); msg != "" {
		s.setFlash(w, r, "err", msg)
	} else {
		s.flashOK(w, r, "flash.ok.node_added")
	}
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/nodes", http.StatusSeeOther)
}

// joinMTUWarning reads a freshly joined node's swarm interface and returns a
// warning when overlay packets (the MTU new networks get, plus VXLAN's 50
// bytes) would not fit through it — the node joined, but large responses to
// and from it would stall. "" when they fit or the node cannot be read; the
// Firewall page shows the same comparison for every node.
func (s *Server) joinMTUWarning(r *http.Request, spec cluster.JoinSpec, name string, addr net.IP) string {
	if addr == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), firewallStatusTimeout)
	defer cancel()
	h, err := firewall.Probe(ctx, firewall.RealRunner{Spec: spec, Timeout: firewallRunnerTimeout}, addr)
	if err != nil || h.SwarmIface == nil || h.SwarmIface.MTU == 0 {
		logFrom(r).Warn("addNode: could not read the new node's swarm interface MTU", "err", err, "node", name)
		return ""
	}
	effective := s.cfg.OverlayMTU
	if effective == 0 {
		effective = docker.DefaultOverlayMTU
	}
	if effective+vxlanOverhead <= h.SwarmIface.MTU {
		return ""
	}
	logFrom(r).Warn("addNode: overlay MTU exceeds the new node's swarm interface", "node", name, "iface", h.SwarmIface.Name, "iface_mtu", h.SwarmIface.MTU, "overlay_mtu", effective)
	return i18n.Tf(r.Context(), "flash.err.node_mtu", name, h.SwarmIface.Name, h.SwarmIface.MTU, effective, h.SwarmIface.MTU-vxlanOverhead)
}

// setNodeAvailability drains/activates a node by its Swarm ID (admin-only).
func (s *Server) setNodeAvailability(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	swarmID := chi.URLParam(r, "nodeID")
	avail := r.FormValue("availability")
	if avail != "active" && avail != "drain" {
		s.flashErrT(w, r, "flash.err.invalid_availability")
		return
	}
	if s.engine == nil {
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	// Never drain the control-plane (leader) node — that breaks scheduling/Krill.
	if avail == "drain" {
		if n, ok := s.findSwarmNode(r.Context(), swarmID); ok && n.Leader {
			s.flashErrT(w, r, "flash.err.protect_manager")
			return
		}
	}
	if err := s.engine.NodeSetAvailability(r.Context(), swarmID, avail); err != nil {
		logFrom(r).Error("setNodeAvailability: failed", "err", err, "node", swarmID, "availability", avail)
		s.flashErrT(w, r, "flash.err.set_availability")
		return
	}
	logFrom(r).Info("node availability set", "node", swarmID, "availability", avail)
	s.flashOK(w, r, "flash.ok.node_availability")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/nodes", http.StatusSeeOther)
}

// removeNode removes a node from the swarm (best-effort) and deletes its managed
// row (admin-only). The row is deleted even if the node is unreachable.
func (s *Server) removeNode(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	swarmID := chi.URLParam(r, "nodeID")
	// Never remove the control-plane (leader) node — that destroys the cluster.
	if n, ok := s.findSwarmNode(r.Context(), swarmID); ok && n.Leader {
		s.flashErrT(w, r, "flash.err.protect_manager")
		return
	}
	// Preflight: unless the operator already confirmed (force=1), warn about any
	// DB instances/apps still pinned to this node instead of silently orphaning
	// them.
	force := r.FormValue("force") == "1"
	if !force {
		var hostname string
		if n, ok := s.findSwarmNode(r.Context(), swarmID); ok {
			hostname = n.Hostname
		}
		var insts []db.ListDBInstancesByNodeHostnameRow
		if hostname != "" {
			insts, _ = s.q.ListDBInstancesByNodeHostname(r.Context(), hostname)
		}
		var affectedApps []db.ListPinnedApplicationsRow
		if pinned, err := s.q.ListPinnedApplications(r.Context()); err == nil {
			for _, a := range pinned {
				if placementContainsNode(a.PlacementNodes, swarmID) {
					affectedApps = append(affectedApps, a)
				}
			}
		}
		if len(insts) > 0 || len(affectedApps) > 0 {
			node, _ := s.findSwarmNode(r.Context(), swarmID)
			node.ID = swarmID // ensure the confirm form action always targets a valid node id
			render(w, r, http.StatusOK, templates.NodeRemoveConfirm(o, role, node, insts, affectedApps))
			return
		}
	}
	// Held through the firewall Open and the row delete: a lockdown running
	// meanwhile could otherwise apply its ruleset to this node after Open
	// stopped its revert timer, leaving it locked with no row to open it from.
	if !s.lockFirewall(w, r) {
		return
	}
	defer s.fwMu.Unlock()
	if s.engine != nil {
		if err := s.engine.NodeRemove(r.Context(), swarmID, true); err != nil {
			logFrom(r).Warn("removeNode: swarm remove failed (deleting row anyway)", "err", err, "node", swarmID)
		}
	}
	rows, lerr := s.q.ListClusterNodes(r.Context())
	if lerr != nil {
		logFrom(r).Error("removeNode: list cluster nodes failed", "err", lerr, "node", swarmID)
	}
	for _, row := range rows {
		if row.SwarmNodeID == swarmID {
			// A locked node reloads its persisted ruleset at every boot, and
			// once its row is gone nothing in Krill can open it again.
			if rr, kerr := firewallRunner(row); kerr == nil {
				if oerr := firewall.Open(r.Context(), rr); oerr != nil {
					logFrom(r).Warn("removeNode: firewall not opened on the removed node", "err", oerr, "node", row.Name)
				}
			} else {
				logFrom(r).Warn("removeNode: node ssh key unusable, firewall not opened", "err", kerr, "node", row.Name)
			}
			if derr := s.q.DeleteClusterNode(r.Context(), row.ID); derr != nil {
				logFrom(r).Error("removeNode: delete row failed", "err", derr, "id", row.ID)
			}
		}
	}
	logFrom(r).Info("cluster node removed", "node", swarmID)
	// A removed node drops out of engine.Nodes() and so out of the
	// observability agent's krill_node mapping too (see cmd/krill's
	// obsNodeNames) if it had a node_labels row — trigger unconditionally
	// rather than looking that up: Reconcile is a no-op when nothing in the
	// rendered config actually changed.
	if s.obsWired() {
		s.obs.ctl.Trigger()
	}
	s.flashOK(w, r, "flash.ok.node_removed")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/nodes", http.StatusSeeOther)
}

// metricsNodeKeys maps each Swarm node ID to the name the metrics sampler
// files its samples under: "control-plane" for the manager Krill runs on, a
// worker's cluster_nodes name otherwise (none for a worker Krill did not add,
// which is not sampled). The display label must not be used for this: it can
// differ, or match another worker's registered name.
func (s *Server) metricsNodeKeys(ctx context.Context, live []docker.SwarmNode) map[string]string {
	keys := map[string]string{}
	workers := map[string]string{}
	if rows, err := s.q.ListClusterNodes(ctx); err == nil {
		for _, r := range rows {
			if r.SwarmNodeID != "" {
				workers[r.SwarmNodeID] = r.Name
			}
		}
	} else {
		slog.Warn("metricsNodeKeys: list cluster nodes", "err", err)
	}
	for _, n := range live {
		switch {
		case n.Leader || n.Role == "manager":
			keys[n.ID] = "control-plane"
		case workers[n.ID] != "":
			keys[n.ID] = workers[n.ID]
		}
	}
	return keys
}
