package server

import (
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
	render(w, r, http.StatusOK, templates.Nodes(o, role, live, rows))
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
	// Resolve the new node's Swarm ID by matching its advertised address.
	if nodes, nerr := s.engine.Nodes(r.Context()); nerr == nil {
		for _, n := range nodes {
			if n.Addr == host {
				_ = s.q.SetClusterNodeSwarmID(r.Context(), db.SetClusterNodeSwarmIDParams{ID: row.ID, SwarmNodeID: n.ID})
				break
			}
		}
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
	if s.engine != nil {
		if err := s.engine.NodeRemove(r.Context(), swarmID, true); err != nil {
			logFrom(r).Warn("removeNode: swarm remove failed (deleting row anyway)", "err", err, "node", swarmID)
		}
	}
	if rows, err := s.q.ListClusterNodes(r.Context()); err == nil {
		for _, row := range rows {
			if row.SwarmNodeID == swarmID {
				if derr := s.q.DeleteClusterNode(r.Context(), row.ID); derr != nil {
					logFrom(r).Error("removeNode: delete row failed", "err", derr, "id", row.ID)
				}
			}
		}
	}
	logFrom(r).Info("cluster node removed", "node", swarmID)
	s.flashOK(w, r, "flash.ok.node_removed")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/nodes", http.StatusSeeOther)
}
