package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

// createLogicalDatabase creates a database inside a running postgres instance of
// this org (form: instance_id, name, db_name?, username?). The physical DB and
// its owner user are provisioned synchronously via psql exec.
func (s *Server) createLogicalDatabase(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return
	}
	instID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("instance_id")), 10, 64)
	if err != nil {
		s.flashErrT(w, r, "flash.err.invalid_database")
		return
	}
	inst, err := s.q.GetDBInstance(r.Context(), instID)
	if err != nil || inst.OrganizationID != o.ID {
		logFrom(r).Info("createLogicalDatabase: instance not in org", "instance_id", instID, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	if inst.Engine != "postgres" {
		s.flashErrT(w, r, "flash.err.ldb_postgres_only")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.flashErrT(w, r, "flash.err.engine_name_required")
		return
	}
	dbName := strings.TrimSpace(r.FormValue("db_name"))
	if dbName == "" {
		dbName = dbservice.SanitizeIdent(name)
	}
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		username = dbName
	}
	if !dbservice.ValidIdent(dbName) || !dbservice.ValidIdent(username) {
		s.flashErrT(w, r, "flash.err.ldb_bad_ident")
		return
	}
	instPW, derr := secret.Dec(inst.SuperuserPassword)
	if derr != nil {
		logFrom(r).Error("createLogicalDatabase: instance password undecryptable", "err", derr, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.secret_undecryptable")
		return
	}
	di := dbservice.Instance{AppName: inst.AppName, Superuser: inst.Superuser, SuperuserPassword: instPW}
	if !s.dbsvc.InstanceRunning(r.Context(), di) {
		s.flashErrT(w, r, "flash.err.ldb_instance_down")
		return
	}
	pw, err := genPassword()
	if err != nil {
		logFrom(r).Error("createLogicalDatabase: password generation failed", "err", err)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	ld, err := s.q.CreateLogicalDatabase(r.Context(), db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: e.ID, Name: name, DbName: dbName, Username: username, Password: secret.Enc(pw),
	})
	if err != nil {
		logFrom(r).Error("createLogicalDatabase: insert failed", "err", err, "environment_id", e.ID, "instance_id", inst.ID, "db_name", dbName)
		s.flashErrErr(w, r, "flash.err.create_generic", err)
		return
	}
	if err := s.dbsvc.ProvisionLogicalDB(r.Context(), di, dbservice.LogicalDB{DBName: dbName, Username: username, Password: pw}); err != nil {
		// The row is useless without the physical DB — roll it back and surface the error.
		if derr := s.q.DeleteLogicalDatabase(r.Context(), ld.ID); derr != nil {
			logFrom(r).Error("createLogicalDatabase: rollback row failed", "err", derr, "ldb_id", ld.ID)
		}
		logFrom(r).Error("createLogicalDatabase: provisioning failed", "err", err, "instance_id", inst.ID, "db_name", dbName)
		s.flashErrErr(w, r, "flash.err.ldb_provision", err)
		return
	}
	logFrom(r).Info("logical database created", "ldb_id", ld.ID, "environment_id", e.ID, "instance_id", inst.ID, "db_name", dbName)
	s.flashOK(w, r, "flash.ok.db_created")
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID)+"?tab=databases", http.StatusSeeOther)
}

// loadLogicalDB parses {dbID} and verifies the logical database belongs to the
// org→proj→env chain (404 on any mismatch).
func (s *Server) loadLogicalDB(w http.ResponseWriter, r *http.Request) (db.LogicalDatabase, bool) {
	_, _, ok := s.loadOrg(w, r)
	if !ok {
		return db.LogicalDatabase{}, false
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return db.LogicalDatabase{}, false
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return db.LogicalDatabase{}, false
	}
	id, ok := pathID(r, "dbID")
	if !ok {
		http.NotFound(w, r)
		return db.LogicalDatabase{}, false
	}
	ld, err := s.q.GetLogicalDatabase(r.Context(), id)
	if err != nil || ld.EnvironmentID != e.ID {
		logFrom(r).Info("loadLogicalDB: not found in environment", "ldb_id", id, "environment_id", e.ID)
		http.NotFound(w, r)
		return db.LogicalDatabase{}, false
	}
	return ld, true
}

func (s *Server) logicalDatabaseDetail(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return
	}
	ld, ok := s.loadLogicalDB(w, r)
	if !ok {
		return
	}
	inst, err := s.q.GetDBInstance(r.Context(), ld.InstanceID)
	if err != nil {
		logFrom(r).Error("logicalDatabaseDetail: instance missing", "err", err, "ldb_id", ld.ID)
		http.NotFound(w, r)
		return
	}
	base := envURL(o.ID, p.ID, e.ID) + "/databases/" + strconv.FormatInt(ld.ID, 10)
	c := templates.LogicalDBCtx{Org: o, Role: role, Project: p, Env: e, LDB: ld, Inst: inst, Base: base}
	di := dbservice.Instance{AppName: inst.AppName, ExternalPort: inst.ExternalPort}
	// A connection string built from an undecryptable password would be copied
	// by the operator and silently fail to authenticate — show none instead.
	if ldPW, derr := secret.Dec(ld.Password); derr != nil {
		logFrom(r).Error("logicalDatabaseDetail: password undecryptable", "err", derr, "ldb_id", ld.ID)
	} else {
		dl := dbservice.LogicalDB{DBName: ld.DbName, Username: ld.Username, Password: ldPW}
		c.Internal = dbservice.PostgresURL("postgresql", di, dl)
		if inst.ExternalPort != nil {
			c.External = dbservice.PostgresExternalURL(di, dl, s.cfg.Host)
		}
	}
	if backups, err := s.q.ListBackupsByLogicalDB(r.Context(), ld.ID); err == nil {
		c.Backups = backups
	}
	if dests, err := s.q.ListDestinationsByOrg(r.Context(), o.ID); err == nil {
		c.Destinations = dests
	}
	render(w, r, http.StatusOK, templates.DatabaseDetail(c))
}

func (s *Server) deleteLogicalDatabase(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return
	}
	ld, ok := s.loadLogicalDB(w, r)
	if !ok {
		return
	}
	inst, err := s.q.GetDBInstance(r.Context(), ld.InstanceID)
	if err != nil {
		logFrom(r).Error("deleteLogicalDatabase: instance missing", "err", err, "ldb_id", ld.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	instPW, derr := secret.Dec(inst.SuperuserPassword)
	if derr != nil {
		logFrom(r).Error("deleteLogicalDatabase: instance password undecryptable", "err", derr, "instance_id", inst.ID)
		s.flashErrT(w, r, "flash.err.secret_undecryptable")
		return
	}
	di := dbservice.Instance{AppName: inst.AppName, Superuser: inst.Superuser, SuperuserPassword: instPW}
	if !s.dbsvc.InstanceRunning(r.Context(), di) {
		s.flashErrT(w, r, "flash.err.ldb_instance_down")
		return
	}
	if err := s.dbsvc.DropLogicalDB(r.Context(), di, dbservice.LogicalDB{DBName: ld.DbName, Username: ld.Username}); err != nil {
		logFrom(r).Error("deleteLogicalDatabase: drop failed", "err", err, "ldb_id", ld.ID)
		s.flashErrErr(w, r, "flash.err.delete_database", err)
		return
	}
	if err := s.q.DeleteLogicalDatabase(r.Context(), ld.ID); err != nil {
		logFrom(r).Error("deleteLogicalDatabase: row delete failed", "err", err, "ldb_id", ld.ID)
		s.flashErrT(w, r, "flash.err.delete_database")
		return
	}
	s.reloadBackupSchedules() // its backups cascade-deleted with the row
	logFrom(r).Info("logical database deleted", "ldb_id", ld.ID, "db_name", ld.DbName)
	s.flashOK(w, r, "flash.ok.db_deleted")
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID)+"?tab=databases", http.StatusSeeOther)
}

func genPassword() (string, error) {
	t, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	if len(t) < 16 {
		return "", fmt.Errorf("generated password too short")
	}
	return t[:16], nil
}
