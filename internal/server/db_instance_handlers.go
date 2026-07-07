package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

func instBase(orgID, instID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/db-servers/" + strconv.FormatInt(instID, 10)
}

// loadInstance parses {instID} and verifies it belongs to the current org (404
// on mismatch — no cross-org capability disclosure).
func (s *Server) loadInstance(w http.ResponseWriter, r *http.Request) (db.DbInstance, bool) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return db.DbInstance{}, false
	}
	id, ok := pathID(r, "instID")
	if !ok {
		http.NotFound(w, r)
		return db.DbInstance{}, false
	}
	inst, err := s.q.GetDBInstance(r.Context(), id)
	if err != nil || inst.OrganizationID != o.ID {
		logFrom(r).Info("loadInstance: not found in org", "instance_id", id, "org_id", o.ID)
		http.NotFound(w, r)
		return db.DbInstance{}, false
	}
	return inst, true
}

func (s *Server) listDBInstances(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	insts, err := s.q.ListDBInstancesByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("listDBInstances: list failed", "err", err, "org_id", o.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var nodes []docker.SwarmNode
	statuses := map[int64]string{}
	if s.engine != nil {
		// Fetched for every viewer (not just admin/owner): the honest status badge
		// needs the live node list to detect node_down/node_removed regardless of
		// role. The node-picker in the template still only renders for admin/owner.
		nodes, _ = s.engine.Nodes(r.Context())
		names := make([]string, 0, len(insts))
		for _, in := range insts {
			names = append(names, in.AppName)
		}
		states, _ := s.engine.ServiceStates(r.Context(), names)
		for _, in := range insts {
			running := false
			if st, ok := states[in.AppName]; ok {
				running = st.Found && st.Desired > 0 && st.Running >= st.Desired
			}
			statuses[in.ID] = displayInstanceStatus(running, in.NodeHostname, nodes, in.Status)
		}
	}
	render(w, r, http.StatusOK, templates.DBServers(o, role, insts, nodes, statuses))
}

func (s *Server) createDBInstance(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	engine := r.FormValue("engine")
	name := strings.TrimSpace(r.FormValue("name"))
	version := strings.TrimSpace(r.FormValue("version"))
	if name == "" || (engine != "postgres" && engine != "redis") {
		s.flashErrT(w, r, "flash.err.engine_name_required")
		return
	}
	var extPort *int32
	if v := strings.TrimSpace(r.FormValue("external_port")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			s.flashErrT(w, r, "flash.err.invalid_external_port")
			return
		}
		x := int32(n)
		instN, _ := s.q.CountDBInstancesByExternalPort(r.Context(), &x)
		apN, _ := s.q.CountAppPortsByHostPort(r.Context(), db.CountAppPortsByHostPortParams{HostPort: x, Protocol: "tcp"})
		if instN+apN > 0 {
			s.flashErrT(w, r, "flash.err.external_port_in_use")
			return
		}
		extPort = &x
	}
	node := strings.TrimSpace(r.FormValue("node_hostname"))
	if node != "" && s.engine != nil {
		live, _ := s.engine.Nodes(r.Context())
		valid := false
		for _, n := range live {
			if n.Hostname == node {
				valid = true
				break
			}
		}
		if !valid {
			s.flashErrT(w, r, "flash.err.invalid_node")
			return
		}
	}
	if version == "" {
		if engine == "postgres" {
			version = "postgres:17"
		} else {
			version = "redis:7"
		}
	}
	su := ""
	if engine == "postgres" {
		su = "postgres"
	}
	pw, err := genPassword()
	if err != nil {
		logFrom(r).Error("createDBInstance: password generation failed", "err", err, "org_id", o.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	app := dbservice.GenerateAppName(engine, name)
	inst, err := s.q.CreateDBInstance(r.Context(), db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: engine, Name: name, AppName: app, Image: version,
		Superuser: su, SuperuserPassword: secret.Enc(pw), ExternalPort: extPort, NodeHostname: node,
	})
	if err != nil {
		logFrom(r).Error("createDBInstance: create failed", "err", err, "org_id", o.ID, "engine", engine, "name", name)
		s.flashErrErr(w, r, "flash.err.create_generic", err)
		return
	}
	logFrom(r).Info("db instance created", "instance_id", inst.ID, "org_id", o.ID, "engine", engine, "app_name", app)
	s.flashOK(w, r, "flash.ok.dbi_created")
	http.Redirect(w, r, instBase(o.ID, inst.ID), http.StatusSeeOther)
}

func (s *Server) dbInstanceDetail(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	ldbs, err := s.q.ListLogicalDatabasesByInstance(r.Context(), inst.ID)
	if err != nil {
		logFrom(r).Error("dbInstanceDetail: list dbs failed", "err", err, "instance_id", inst.ID)
	}
	c := templates.DBServerCtx{Org: o, Role: role, Inst: inst, DBs: ldbs, Base: instBase(o.ID, inst.ID)}
	if role == "owner" || role == "admin" {
		if s.engine != nil {
			c.Nodes, _ = s.engine.Nodes(r.Context())
		}
		di := dbservice.Instance{AppName: inst.AppName, Superuser: inst.Superuser,
			SuperuserPassword: secret.Dec(inst.SuperuserPassword), ExternalPort: inst.ExternalPort}
		if inst.Engine == "postgres" {
			c.SuperuserURL = dbservice.PostgresURL("postgresql", di, dbservice.LogicalDB{DBName: "postgres", Username: di.Superuser, Password: di.SuperuserPassword})
		} else {
			c.SuperuserURL = dbservice.RedisInternalURL(di)
			if inst.ExternalPort != nil {
				c.ExternalURL = dbservice.RedisExternalURL(di, s.cfg.Host)
			}
		}
	}
	render(w, r, http.StatusOK, templates.DBServerDetail(c))
}

func (s *Server) deployDBInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	s.dbsvc.DeployInstance(inst.ID)
	logFrom(r).Info("db instance deploy requested", "instance_id", inst.ID)
	s.flashOK(w, r, "flash.ok.deploy_queued")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

func (s *Server) startDBInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	if s.engine == nil {
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if err := s.dbsvc.StartInstance(r.Context(), inst.ID); err != nil {
		logFrom(r).Error("startDBInstance: failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.start_database")
		return
	}
	s.flashOK(w, r, "flash.ok.start_requested")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

func (s *Server) stopDBInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	if s.engine == nil {
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if err := s.dbsvc.StopInstance(r.Context(), inst.ID); err != nil {
		logFrom(r).Error("stopDBInstance: failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.stop_database")
		return
	}
	s.flashOK(w, r, "flash.ok.stop_requested")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

func (s *Server) versionDBInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	image := strings.TrimSpace(r.FormValue("image"))
	if image != "" {
		if err := s.q.UpdateDBInstanceImage(r.Context(), db.UpdateDBInstanceImageParams{ID: inst.ID, Image: image}); err != nil {
			logFrom(r).Error("versionDBInstance: update failed", "err", err, "instance_id", inst.ID, "image", image)
			s.flashErrT(w, r, "flash.err.update_image")
			return
		}
		logFrom(r).Info("db instance version updated", "instance_id", inst.ID, "image", image)
	}
	s.dbsvc.DeployInstance(inst.ID)
	s.flashOK(w, r, "flash.ok.version_queued")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// setDBInstanceExternalPort toggles the instance's external port. Empty clears
// it (no external access). A set port is range- and conflict-checked, then the
// instance is redeployed (drops the port from the DB service and reconciles the
// control-plane proxy). Admin-gated; tenant-chained via loadInstance.
func (s *Server) setDBInstanceExternalPort(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	var ext *int32
	if v := strings.TrimSpace(r.FormValue("external_port")); v != "" {
		x, err := strconv.Atoi(v)
		if err != nil || x < 1 || x > 65535 {
			s.flashErrT(w, r, "flash.err.invalid_external_port")
			return
		}
		x32 := int32(x)
		instN, _ := s.q.CountOtherDBInstancesByExternalPort(r.Context(), db.CountOtherDBInstancesByExternalPortParams{ExternalPort: &x32, ID: inst.ID})
		portN, _ := s.q.CountAppPortsByHostPort(r.Context(), db.CountAppPortsByHostPortParams{HostPort: x32, Protocol: "tcp"})
		if instN > 0 || portN > 0 {
			s.flashErrT(w, r, "flash.err.external_port_in_use")
			return
		}
		ext = &x32
	}
	if err := s.q.UpdateDBInstanceExternalPort(r.Context(), db.UpdateDBInstanceExternalPortParams{ID: inst.ID, ExternalPort: ext}); err != nil {
		logFrom(r).Error("setDBInstanceExternalPort: update failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	s.dbsvc.DeployInstance(inst.ID)
	logFrom(r).Info("db instance external port set", "instance_id", inst.ID, "external", ext != nil)
	s.flashOK(w, r, "flash.ok.external_access_updated")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

func (s *Server) setDBInstanceNode(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	node := strings.TrimSpace(r.FormValue("node_hostname"))
	if node != "" && s.engine != nil {
		live, _ := s.engine.Nodes(r.Context())
		valid := false
		for _, n := range live {
			if n.Hostname == node {
				valid = true
				break
			}
		}
		if !valid {
			s.flashErrT(w, r, "flash.err.invalid_node")
			return
		}
	}
	if err := s.q.SetDBInstanceNode(r.Context(), db.SetDBInstanceNodeParams{ID: inst.ID, NodeHostname: node}); err != nil {
		logFrom(r).Error("setDBInstanceNode: update failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	logFrom(r).Info("db instance node set", "instance_id", inst.ID, "node", node)
	s.flashOK(w, r, "flash.ok.db_node_saved")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// deleteDBInstance refuses while logical databases exist unless the admin
// explicitly confirms cascade (delete_databases=on). destroy_data additionally
// removes the volume, mirroring the legacy managed-DB delete. dbservice.DeleteInstance
// is engine-gated internally (nil engine → row-only delete), so this handler
// works in tests without an engine.
func (s *Server) deleteDBInstance(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	n, err := s.q.CountLogicalDatabasesByInstance(r.Context(), inst.ID)
	if err != nil {
		logFrom(r).Error("deleteDBInstance: count dbs failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if n > 0 {
		if r.FormValue("delete_databases") != "on" {
			s.flashErrT(w, r, "flash.err.dbi_has_databases")
			return
		}
		ldbs, _ := s.q.ListLogicalDatabasesByInstance(r.Context(), inst.ID)
		for _, ld := range ldbs {
			if derr := s.q.DeleteLogicalDatabase(r.Context(), ld.ID); derr != nil {
				logFrom(r).Error("deleteDBInstance: delete logical db row failed", "err", derr, "ldb_id", ld.ID)
				s.flashErrT(w, r, "flash.err.internal")
				return
			}
		}
	}
	destroy := r.FormValue("destroy_data") == "on"
	if err := s.dbsvc.DeleteInstance(r.Context(), inst.ID, destroy); err != nil {
		logFrom(r).Error("deleteDBInstance: failed", "err", err, "instance_id", inst.ID, "destroy_data", destroy)
		s.flashErrT(w, r, "flash.err.delete_database")
		return
	}
	logFrom(r).Info("db instance deleted", "instance_id", inst.ID, "destroy_data", destroy)
	s.flashOK(w, r, "flash.ok.dbi_deleted")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/db-servers", http.StatusSeeOther)
}

func (s *Server) dbInstanceStatus(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	status := inst.Status
	if s.engine != nil {
		running := false
		if st, err := s.engine.ServiceState(r.Context(), inst.AppName); err == nil && st.Found {
			running = st.Running >= st.Desired && st.Desired > 0
		}
		live, _ := s.engine.Nodes(r.Context())
		status = displayInstanceStatus(running, inst.NodeHostname, live, inst.Status)
	}
	render(w, r, http.StatusOK, templates.StatusBadge(status))
}

// dbInstanceLogs streams live container logs over a WebSocket. Adapted from
// databaseLogs (db_handlers.go) for the instance model.
func (s *Server) dbInstanceLogs(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	if s.engine == nil {
		logFrom(r).Error("dbInstanceLogs: engine unavailable", "instance_id", inst.ID)
		http.Error(w, "engine unavailable", http.StatusServiceUnavailable)
		return
	}
	appName := inst.AppName
	release, ok := s.acquireLogSlot()
	if !ok {
		http.Error(w, "too many live log streams, try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer release()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Error("dbInstanceLogs: websocket accept failed", "err", err, "instance_id", inst.ID, "app_name", appName)
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())
	rc, err := s.engine.ServiceLogs(ctx, appName, true)
	if err != nil {
		logFrom(r).Error("dbInstanceLogs: engine service logs failed", "err", err, "instance_id", inst.ID, "app_name", appName)
		conn.Close(websocket.StatusInternalError, "logs unavailable")
		return
	}
	defer rc.Close()
	streamParsedLogsToWS(ctx, conn, rc)
}

// dbInstanceDeployLogs streams the in-memory deploy-log feed over a WebSocket.
// Adapted from databaseDeployLogs (db_handlers.go) for the instance model.
func (s *Server) dbInstanceDeployLogs(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	feed := dbservice.InstanceFeedID(inst.ID)
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Error("dbInstanceDeployLogs: websocket accept failed", "err", err, "instance_id", inst.ID)
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())
	if s.logHub != nil && s.logHub.Active(feed) {
		sub := s.logHub.Subscribe(feed)
		defer s.logHub.Unsubscribe(feed, sub)
		for {
			select {
			case <-ctx.Done():
				return
			case line, open := <-sub:
				if !open {
					conn.Close(websocket.StatusNormalClosure, "")
					return
				}
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := conn.Write(wctx, websocket.MessageText, []byte(line)); err != nil {
					cancel()
					return
				}
				cancel()
			}
		}
	}
	conn.Write(ctx, websocket.MessageText, []byte("(no active deploy)\n"))
	conn.Close(websocket.StatusNormalClosure, "")
}
