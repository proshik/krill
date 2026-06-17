package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/proshik/krill/internal/cluster"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
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
	var live []docker.SwarmNode
	if s.engine != nil {
		live, _ = s.engine.Nodes(r.Context())
	}
	rows, err := s.q.ListClusterNodes(r.Context())
	if err != nil {
		logFrom(r).Error("listNodes: list cluster nodes failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.Nodes(o, role, live, rows, s.nodeLabelMap(r.Context())))
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
	_, hostKey, jerr := cluster.Join(cluster.JoinSpec{
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
		logFrom(r).Error("addNode: create node row failed", "err", cerr, "name", name)
		s.flashErrErr(w, r, "flash.err.node_join", cerr)
		return
	}
	// Resolve the new node's Swarm ID by matching its advertised address OR
	// hostname (the operator may have entered either). Warn if unresolved so the
	// blank swarm_node_id (un-removable via UI) is observable, not silent.
	resolved := false
	if nodes, nerr := s.engine.Nodes(r.Context()); nerr == nil {
		for _, n := range nodes {
			if n.Addr == host || n.Hostname == host {
				_ = s.q.SetClusterNodeSwarmID(r.Context(), db.SetClusterNodeSwarmIDParams{ID: row.ID, SwarmNodeID: n.ID})
				resolved = true
				break
			}
		}
	}
	if !resolved {
		logFrom(r).Warn("addNode: joined node not matched to a swarm ID (Addr/hostname mismatch?)", "host", host, "id", row.ID)
	}
	logFrom(r).Info("cluster node added", "name", name, "host", host) // no key/token
	s.flashOK(w, r, "flash.ok.node_added")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/nodes", http.StatusSeeOther)
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
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	swarmID := chi.URLParam(r, "nodeID")
	// Never remove the control-plane (leader) node — that destroys the cluster.
	if n, ok := s.findSwarmNode(r.Context(), swarmID); ok && n.Leader {
		s.flashErrT(w, r, "flash.err.protect_manager")
		return
	}
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
			if derr := s.q.DeleteClusterNode(r.Context(), row.ID); derr != nil {
				logFrom(r).Error("removeNode: delete row failed", "err", derr, "id", row.ID)
			}
		}
	}
	logFrom(r).Info("cluster node removed", "node", swarmID)
	s.flashOK(w, r, "flash.ok.node_removed")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/nodes", http.StatusSeeOther)
}
