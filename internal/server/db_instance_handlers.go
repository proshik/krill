package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

// minioRootUserRe validates the user-chosen MinIO root user (MINIO_ROOT_USER):
// alphanumeric only, 3-63 chars (MinIO itself requires >=3 chars).
var minioRootUserRe = regexp.MustCompile(`^[a-zA-Z0-9]{3,63}$`)

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
		var nerr error
		nodes, nerr = s.engine.Nodes(r.Context())
		nodesOK := nerr == nil
		if nerr != nil {
			logFrom(r).Error("listDBInstances: node list unavailable, node badges fall back to stored status", "err", nerr)
		}
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
			statuses[in.ID] = displayInstanceStatus(running, in.NodeHostname, nodes, nodesOK, in.Status)
		}
	}
	render(w, r, http.StatusOK, templates.DBServers(o, role, insts, nodes, statuses))
}

// parseInstancePort parses an optional host-port form value: empty input is
// valid (ok=true, port=nil, meaning "unset"); a non-empty value must be a
// plain integer in 1..65535.
func parseInstancePort(v string) (port *int32, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return nil, false
	}
	x := int32(n)
	return &x, true
}

// dbInstancePortInUse reports whether port collides with another db_instances
// row's external_port OR console_external_port (both are host-published on
// the manager and share one port namespace — CountDBInstancesByExternalPort
// checks both columns) or an app's raw published TCP port. Used when creating
// a new instance (no row of its own yet to exclude).
// A query failure is reported, never swallowed: treating it as "free" lets two
// instances claim the same host port, and the collision only surfaces later as
// a service that cannot publish.
func (s *Server) dbInstancePortInUse(ctx context.Context, port int32) (bool, error) {
	instN, err := s.q.CountDBInstancesByExternalPort(ctx, &port)
	if err != nil {
		return false, fmt.Errorf("count db instances on port %d: %w", port, err)
	}
	apN, err := s.q.CountAppPortsByHostPort(ctx, db.CountAppPortsByHostPortParams{HostPort: port, Protocol: "tcp"})
	if err != nil {
		return false, fmt.Errorf("count app ports on port %d: %w", port, err)
	}
	return instN+apN > 0, nil
}

// dbInstancePortInUseByOther is dbInstancePortInUse excluding selfID's own
// row — used when editing an existing instance's ports.
func (s *Server) dbInstancePortInUseByOther(ctx context.Context, port int32, selfID int64) (bool, error) {
	instN, err := s.q.CountOtherDBInstancesByExternalPort(ctx, db.CountOtherDBInstancesByExternalPortParams{ExternalPort: &port, ID: selfID})
	if err != nil {
		return false, fmt.Errorf("count other db instances on port %d: %w", port, err)
	}
	apN, err := s.q.CountAppPortsByHostPort(ctx, db.CountAppPortsByHostPortParams{HostPort: port, Protocol: "tcp"})
	if err != nil {
		return false, fmt.Errorf("count app ports on port %d: %w", port, err)
	}
	return instN+apN > 0, nil
}

func (s *Server) createDBInstance(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	engine := r.FormValue("engine")
	name := strings.TrimSpace(r.FormValue("name"))
	version := strings.TrimSpace(r.FormValue("version"))
	drv, engineOK := drivers.Registry.Get(engine)
	if name == "" || !engineOK {
		s.flashErrT(w, r, "flash.err.engine_name_required")
		return
	}
	extPort, ok := parseInstancePort(r.FormValue("external_port"))
	if !ok {
		s.flashErrT(w, r, "flash.err.invalid_external_port")
		return
	}
	if extPort != nil {
		inUse, perr := s.dbInstancePortInUse(r.Context(), *extPort)
		if perr != nil {
			logFrom(r).Error("createDBInstance: external port conflict check failed", "err", perr, "port", *extPort)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		if inUse {
			s.flashErrT(w, r, "flash.err.external_port_in_use")
			return
		}
	}
	// console_external_port is minio's second target (data + console pair) —
	// the create form only submits it when engine=minio (JS-revealed field);
	// parsing it unconditionally is harmless for single-target engines
	// (postgres/redis/dragonfly), whose form never sends it.
	consolePort, ok := parseInstancePort(r.FormValue("console_external_port"))
	if !ok {
		s.flashErrT(w, r, "flash.err.invalid_console_port")
		return
	}
	if consolePort != nil {
		inUse, perr := s.dbInstancePortInUse(r.Context(), *consolePort)
		if perr != nil {
			logFrom(r).Error("createDBInstance: console port conflict check failed", "err", perr, "port", *consolePort)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		if inUse {
			s.flashErrT(w, r, "flash.err.console_port_in_use")
			return
		}
	}
	if extPort != nil && consolePort != nil && *extPort == *consolePort {
		s.flashErrT(w, r, "flash.err.console_port_in_use")
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
	if version == "" {
		version = drv.DefaultImage()
	}
	su := drv.SuperuserName()
	if engine == "minio" {
		root := strings.TrimSpace(r.FormValue("root_user"))
		if !minioRootUserRe.MatchString(root) {
			s.flashErrT(w, r, "flash.err.minio_root_user")
			return
		}
		su = root
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
		ConsoleExternalPort: consolePort,
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
	c := templates.DBServerCtx{Org: o, Role: role, Inst: inst, DBs: ldbs, Base: instBase(o.ID, inst.ID), Host: s.cfg.Host}
	if role == "owner" || role == "admin" {
		if s.engine != nil {
			c.Nodes, _ = s.engine.Nodes(r.Context())
		}
		instPW, derr := secret.Dec(inst.SuperuserPassword)
		if derr != nil {
			// Leave c.Conn empty rather than render a connection string with a
			// blank password that the operator would copy and fail to use.
			logFrom(r).Error("dbInstanceDetail: superuser password undecryptable", "err", derr, "instance_id", inst.ID)
		} else if drv, ok := drivers.Registry.Get(inst.Engine); ok {
			di := drivers.Instance{
				AppName: inst.AppName, Superuser: inst.Superuser,
				SuperuserPassword:   instPW,
				ExternalPort:        inst.ExternalPort,
				ConsoleExternalPort: inst.ConsoleExternalPort,
			}
			c.Conn = drv.ConnDisplay(di, s.cfg.Host)
		}
	}
	render(w, r, http.StatusOK, templates.DBServerDetail(c))
}

func (s *Server) deployDBInstance(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	// A live migration holds the instance's oplock: any service mutation
	// mid-copy can corrupt the target volume (or restart the DB onto the
	// source while tar reads it), so every lifecycle handler refuses while it
	// is held. In-memory lock, not DB status: status can go stale after a
	// crash; the boot sweep resets it.
	if oplock.Held(oplock.DBInstance(inst.AppName)) {
		s.flashErrT(w, r, "flash.err.migrate_in_progress")
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
	if oplock.Held(oplock.DBInstance(inst.AppName)) {
		s.flashErrT(w, r, "flash.err.migrate_in_progress")
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
	if oplock.Held(oplock.DBInstance(inst.AppName)) {
		s.flashErrT(w, r, "flash.err.migrate_in_progress")
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
	if oplock.Held(oplock.DBInstance(inst.AppName)) {
		s.flashErrT(w, r, "flash.err.migrate_in_progress")
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

// setDBInstanceExternalPort toggles the instance's external port(s). Empty
// clears a port (no external access on that target). Each set port is range-
// and conflict-checked (against every other instance's external/console port
// and app_ports), then the instance is redeployed (drops/adds the port(s)
// from the DB service and reconciles the control-plane proxy/proxies).
// console_external_port is minio-relevant (data + console pair) but parsed
// unconditionally — harmless for engines whose driver ignores it. Admin-gated;
// tenant-chained via loadInstance.
func (s *Server) setDBInstanceExternalPort(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	if oplock.Held(oplock.DBInstance(inst.AppName)) {
		s.flashErrT(w, r, "flash.err.migrate_in_progress")
		return
	}
	ext, ok := parseInstancePort(r.FormValue("external_port"))
	if !ok {
		s.flashErrT(w, r, "flash.err.invalid_external_port")
		return
	}
	if ext != nil {
		inUse, perr := s.dbInstancePortInUseByOther(r.Context(), *ext, inst.ID)
		if perr != nil {
			logFrom(r).Error("setDBInstancePorts: external port conflict check failed", "err", perr, "port", *ext, "instance_id", inst.ID)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		if inUse {
			s.flashErrT(w, r, "flash.err.external_port_in_use")
			return
		}
	}
	// console_external_port: no current form submits this field (no engine
	// uses it yet — it's minio-relevant, landing in a later task), so treat it
	// as "unchanged" unless the request actually includes it — an absent field
	// must not silently wipe a console port a future form didn't mean to touch.
	console := inst.ConsoleExternalPort
	if r.Form.Has("console_external_port") {
		c, ok := parseInstancePort(r.FormValue("console_external_port"))
		if !ok {
			s.flashErrT(w, r, "flash.err.invalid_console_port")
			return
		}
		console = c
	}
	if console != nil {
		inUse, perr := s.dbInstancePortInUseByOther(r.Context(), *console, inst.ID)
		if perr != nil {
			logFrom(r).Error("setDBInstancePorts: console port conflict check failed", "err", perr, "port", *console, "instance_id", inst.ID)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		if inUse {
			s.flashErrT(w, r, "flash.err.console_port_in_use")
			return
		}
	}
	if ext != nil && console != nil && *ext == *console {
		s.flashErrT(w, r, "flash.err.console_port_in_use")
		return
	}
	if err := s.q.UpdateDBInstanceExternalPort(r.Context(), db.UpdateDBInstanceExternalPortParams{ID: inst.ID, ExternalPort: ext}); err != nil {
		logFrom(r).Error("setDBInstanceExternalPort: update failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if err := s.q.UpdateDBInstanceConsolePort(r.Context(), db.UpdateDBInstanceConsolePortParams{ID: inst.ID, ConsoleExternalPort: console}); err != nil {
		logFrom(r).Error("setDBInstanceExternalPort: update console port failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	s.dbsvc.DeployInstance(inst.ID)
	logFrom(r).Info("db instance external port set", "instance_id", inst.ID, "external", ext != nil, "console", console != nil)
	s.flashOK(w, r, "flash.ok.external_access_updated")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// migrateDBInstanceNode moves a DB instance to another node WITH its data
// (async volume migration; see dbservice.MigrateInstanceNode). With no engine
// (tests/single-node bootstrap) it falls back to the legacy metadata-only
// write — there is nothing to migrate without a swarm.
func (s *Server) migrateDBInstanceNode(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	node := strings.TrimSpace(r.FormValue("node_hostname"))
	if s.engine == nil {
		if err := s.q.SetDBInstanceNode(r.Context(), db.SetDBInstanceNodeParams{ID: inst.ID, NodeHostname: node}); err != nil {
			logFrom(r).Error("migrateDBInstanceNode: metadata update failed", "err", err, "instance_id", inst.ID)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
		s.flashOK(w, r, "flash.ok.db_node_saved")
		http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
		return
	}
	// Fetched once up front (not just when node != "") so the same-node check
	// below has the live node list available even for a control-plane target.
	live, _ := s.engine.Nodes(r.Context())
	if node != "" {
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
	if inst.Status == "migrating" {
		s.flashErrT(w, r, "flash.err.migrate_in_progress")
		return
	}
	if node == inst.NodeHostname {
		// Refuse only when the metadata is corroborated: no running task
		// (metadata is all we have) or the task actually runs where the
		// metadata says. A contradicting live task means the metadata is
		// stale (the legacy metadata-only node change) and re-selecting
		// the same target is the healing migration — let it through.
		corroborated := true
		if tasks, terr := s.engine.ServiceTasks(r.Context(), inst.AppName); terr == nil {
			for _, t := range tasks {
				if t.State == "running" {
					want := node
					if want == "" { // metadata "" = control-plane; compare against the leader's hostname
						for _, n := range live {
							if n.Leader {
								want = n.Hostname
								break
							}
						}
					}
					corroborated = t.NodeName == want
					break
				}
			}
		}
		if corroborated {
			s.flashErrT(w, r, "flash.err.same_node")
			return
		}
	}
	deleteSource := r.FormValue("delete_source") == "on"
	s.dbsvc.MigrateInstanceNode(inst.ID, node, deleteSource)
	logFrom(r).Info("db instance migration started", "instance_id", inst.ID, "target", node, "delete_source", deleteSource)
	s.flashOK(w, r, "flash.ok.db_migration_started")
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// deleteDBInstance refuses while logical databases exist unless the admin
// explicitly confirms cascade (delete_databases=on). destroy_data additionally
// removes the volume, mirroring the legacy managed-DB delete. It runs in three
// steps: dbsvc.RemoveInstanceContainers first (the real-failure point — service
// +optional volume teardown), then the logical_databases rows (their ON DELETE
// RESTRICT FK to db_instances would otherwise block the instance row anyway),
// then dbsvc.DeleteInstanceRow. RemoveInstanceContainers is engine-gated
// internally (nil engine → no-op), so this handler works in tests without an
// engine.
func (s *Server) deleteDBInstance(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	inst, ok := s.loadInstance(w, r)
	if !ok {
		return
	}
	if oplock.Held(oplock.DBInstance(inst.AppName)) {
		s.flashErrT(w, r, "flash.err.migrate_in_progress")
		return
	}
	n, err := s.q.CountLogicalDatabasesByInstance(r.Context(), inst.ID)
	if err != nil {
		logFrom(r).Error("deleteDBInstance: count dbs failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	if n > 0 && r.FormValue("delete_databases") != "on" {
		s.flashErrT(w, r, "flash.err.dbi_has_databases")
		return
	}
	destroy := r.FormValue("destroy_data") == "on"

	// Remove the instance's containers FIRST — the real-failure point — before
	// touching any DB rows. logical_databases has an ON DELETE RESTRICT FK to
	// db_instances, so those rows (and their backups/links) can't be dropped
	// before the containers are confirmed gone anyway; sequencing it this way
	// also means a real docker failure here never silently destroys bookkeeping
	// for an instance that is still actually running (not-found is tolerated —
	// see dbsvc.RemoveInstanceContainers).
	if err := s.dbsvc.RemoveInstanceContainers(r.Context(), inst.ID, destroy); err != nil {
		logFrom(r).Error("deleteDBInstance: failed", "err", err, "instance_id", inst.ID, "destroy_data", destroy)
		s.flashErrT(w, r, "flash.err.delete_database")
		return
	}

	if n > 0 {
		ldbs, _ := s.q.ListLogicalDatabasesByInstance(r.Context(), inst.ID)
		for _, ld := range ldbs {
			if derr := s.q.DeleteLogicalDatabase(r.Context(), ld.ID); derr != nil {
				logFrom(r).Error("deleteDBInstance: delete logical db row failed", "err", derr, "ldb_id", ld.ID)
				s.flashErrT(w, r, "flash.err.internal")
				return
			}
		}
	}

	if err := s.dbsvc.DeleteInstanceRow(r.Context(), inst.ID); err != nil {
		logFrom(r).Error("deleteDBInstance: delete instance row failed", "err", err, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}

	s.reloadBackupSchedules() // the instance's logical databases took their backups with them
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
		live, nerr := s.engine.Nodes(r.Context())
		status = displayInstanceStatus(running, inst.NodeHostname, live, nerr == nil, inst.Status)
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
	release, ok := s.acquireLogSlotFor(auth.UserID(r.Context()))
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
	rc, err := s.engine.ServiceLogs(ctx, appName, true, 200) // 200: the viewer's live-tail window, unchanged from before tail was parameterized
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
