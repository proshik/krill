package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

// removeEnvDatabases drops the environment's logical databases inside their
// instances (best-effort: an unreachable instance leaves a physical orphan,
// logged; the rows cascade with the environment). Instances are org-level and
// are NOT touched.
func (s *Server) removeEnvDatabases(ctx context.Context, envID int64) {
	if s.dbsvc == nil {
		return
	}
	ldbs, _ := s.q.ListLogicalDatabasesByEnvironment(ctx, envID)
	for _, ld := range ldbs {
		inst, err := s.q.GetDBInstance(ctx, ld.InstanceID)
		if err != nil {
			slog.Error("removeEnvDatabases: instance lookup", "ldb", ld.ID, "err", err)
			continue
		}
		instPW, derr := secret.Dec(inst.SuperuserPassword)
		if derr != nil {
			// Without the superuser password the DROP cannot run; skip rather
			// than delete the row and orphan the physical database silently.
			slog.Error("removeEnvDatabases: instance password undecryptable (physical database left in place)", "ldb", ld.ID, "instance", inst.ID, "err", derr)
			continue
		}
		di := dbservice.Instance{AppName: inst.AppName, Superuser: inst.Superuser, SuperuserPassword: instPW}
		if err := s.dbsvc.DropLogicalDB(ctx, di, dbservice.LogicalDB{DBName: ld.DbName, Username: ld.Username}); err != nil {
			slog.Error("removeEnvDatabases: drop logical db (physical orphan left)", "ldb", ld.ID, "db_name", ld.DbName, "err", err)
		}
		if err := s.q.DeleteLogicalDatabase(ctx, ld.ID); err != nil {
			slog.Error("removeEnvDatabases: delete row", "ldb", ld.ID, "err", err)
		}
	}
}

// removeEnvAppVolumes deletes the Docker volumes of every application in an
// environment. Used by environment/project delete so app volumes are not
// orphaned — the rows cascade-delete, but without this the Docker volumes linger
// with no UI to clean them (symmetry with removeEnvDatabases).
func (s *Server) removeEnvAppVolumes(ctx context.Context, envID int64) {
	if s.engine == nil {
		return
	}
	apps, _ := s.q.ListApplicationsByEnvironment(ctx, envID)
	for _, a := range apps {
		vols, _ := s.q.ListVolumesByApplication(ctx, a.ID)
		for _, v := range vols {
			s.removeAppVolume(ctx, docker.VolumeName(a.ID, v.Name))
		}
	}
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, err := s.org.CreateProject(r.Context(), o.ID, r.FormValue("name"), r.FormValue("description"))
	if err != nil {
		logFrom(r).Info("createProject: rejected", "err", err, "org_id", o.ID)
		s.flashErrErr(w, r, "flash.err.create_project", err)
		return
	}
	logFrom(r).Info("project created", "project_id", p.ID, "org_id", o.ID, "slug", p.Slug)
	s.flashOK(w, r, "flash.ok.project_created")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10), http.StatusSeeOther)
}

func (s *Server) renameProject(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	if err := s.org.RenameProject(r.Context(), p.ID, r.FormValue("name")); err != nil {
		logFrom(r).Info("renameProject: rejected", "err", err, "project_id", p.ID)
		s.flashErrErr(w, r, "flash.err.rename_project", err)
		return
	}
	logFrom(r).Info("project renamed", "project_id", p.ID, "org_id", o.ID)
	s.flashOK(w, r, "flash.ok.project_renamed")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	// remove Swarm services of all applications in the project
	envs, err := s.q.ListEnvironments(r.Context(), p.ID)
	if err != nil {
		slog.Error("deleteProject: list environments", "project", p.ID, "err", err)
	}
	for _, e := range envs {
		apps, err := s.q.ListApplicationsByEnvironment(r.Context(), e.ID)
		if err != nil {
			slog.Error("deleteProject: list apps", "env", e.ID, "err", err)
		}
		for _, a := range apps {
			if s.engine != nil {
				if err := s.engine.ServiceRemove(r.Context(), dockerName(a.ID)); err != nil {
					slog.Error("deleteProject: service remove", "app", a.ID, "err", err)
				}
			}
		}
		s.removeEnvAppVolumes(r.Context(), e.ID)
		s.removeEnvDatabases(r.Context(), e.ID)
	}
	if err := s.q.DeleteProject(r.Context(), p.ID); err != nil {
		logFrom(r).Error("deleteProject: delete project", "err", err, "project_id", p.ID, "org_id", o.ID)
		s.flashErrT(w, r, "flash.err.delete_project")
		return
	}
	// Everything below the project cascaded away: logical databases (→ backups)
	// and app volumes (→ volume_backups).
	s.reloadBackupSchedules()
	s.reloadVolumeBackupSchedules()
	logFrom(r).Info("project deleted", "project_id", p.ID, "org_id", o.ID, "slug", p.Slug)
	s.flashOK(w, r, "flash.ok.project_deleted")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10), http.StatusSeeOther)
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	e, err := s.org.CreateEnvironment(r.Context(), p.ID, r.FormValue("name"))
	if err != nil {
		logFrom(r).Info("createEnvironment: rejected", "err", err, "project_id", p.ID, "org_id", o.ID)
		s.flashErrErr(w, r, "flash.err.create_environment", err)
		return
	}
	logFrom(r).Info("environment created", "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID, "slug", e.Slug)
	s.flashOK(w, r, "flash.ok.env_created")
	http.Redirect(w, r, projURL(o.ID, p.ID), http.StatusSeeOther)
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
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
	apps, err := s.q.ListApplicationsByEnvironment(r.Context(), e.ID)
	if err != nil {
		slog.Error("deleteEnvironment: list apps", "env", e.ID, "err", err)
	}
	for _, a := range apps {
		if s.engine != nil {
			if err := s.engine.ServiceRemove(r.Context(), dockerName(a.ID)); err != nil {
				slog.Error("deleteEnvironment: service remove", "app", a.ID, "err", err)
			}
		}
	}
	s.removeEnvAppVolumes(r.Context(), e.ID)
	s.removeEnvDatabases(r.Context(), e.ID)
	if err := s.q.DeleteEnvironment(r.Context(), e.ID); err != nil {
		logFrom(r).Error("deleteEnvironment: delete environment", "err", err, "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID)
		s.flashErrT(w, r, "flash.err.delete_environment")
		return
	}
	s.reloadBackupSchedules()
	s.reloadVolumeBackupSchedules()
	logFrom(r).Info("environment deleted", "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID, "slug", e.Slug)
	s.flashOK(w, r, "flash.ok.env_deleted")
	http.Redirect(w, r, projURL(o.ID, p.ID), http.StatusSeeOther)
}

// projectPage displays the project with the selected (or first) environment.
func (s *Server) projectPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	envs, err := s.q.ListEnvironments(r.Context(), p.ID)
	if err != nil {
		logFrom(r).Error("projectPage: list environments", "err", err, "project_id", p.ID, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var active db.Environment
	if envID, ok := pathID(r, "envID"); ok {
		for _, e := range envs {
			if e.ID == envID {
				active = e
			}
		}
		if active.ID == 0 {
			http.NotFound(w, r)
			return
		}
	} else if len(envs) > 0 {
		active = envs[0]
	}
	var apps []db.Application
	var dbs []db.ListLogicalDatabasesByEnvironmentRow
	if active.ID != 0 {
		apps, _ = s.q.ListApplicationsByEnvironment(r.Context(), active.ID)
		dbs, _ = s.q.ListLogicalDatabasesByEnvironment(r.Context(), active.ID)
		// Reflect the LIVE Swarm state on the cards (the stored status can be
		// stale) — one bulk call, not one per card.
		s.deriveCardStatuses(r.Context(), apps, dbs)
	}
	registries, err := s.q.ListRegistriesByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("projectPage: list registries", "err", err, "org_id", o.ID)
	}
	var pgInsts []db.DbInstance
	if all, err := s.q.ListDBInstancesByOrg(r.Context(), o.ID); err == nil {
		for _, in := range all {
			if in.Engine == "postgres" {
				pgInsts = append(pgInsts, in)
			}
		}
	} else {
		logFrom(r).Error("projectPage: list db instances", "err", err, "org_id", o.ID)
	}
	render(w, r, http.StatusOK, templates.Project(o, role, p, envs, active, apps, dbs, pgInsts, registries))
}

// deriveCardStatuses replaces each app/logical-db stored status with the LIVE
// Swarm status of its underlying service, using a single bulk ServiceStates
// call (instead of one per card). Engine-nil-safe; unknown services keep their
// stored status (DeriveStatus). A logical database's card reflects the status
// of its instance's service (InstanceAppName), shared by every database on
// that instance.
func (s *Server) deriveCardStatuses(ctx context.Context, apps []db.Application, ldbs []db.ListLogicalDatabasesByEnvironmentRow) {
	if s.engine == nil {
		return
	}
	names := make([]string, 0, len(apps)+len(ldbs))
	for _, a := range apps {
		names = append(names, dockerName(a.ID))
	}
	for _, ld := range ldbs {
		names = append(names, ld.InstanceAppName)
	}
	states, err := s.engine.ServiceStates(ctx, names)
	if err != nil {
		slog.Error("deriveCardStatuses: bulk service states", "err", err)
		return
	}
	for i := range apps {
		apps[i].Status = deploy.DeriveStatus(states[dockerName(apps[i].ID)], apps[i].Status)
	}
	for i := range ldbs {
		ldbs[i].InstanceStatus = deploy.DeriveStatus(states[ldbs[i].InstanceAppName], ldbs[i].InstanceStatus)
	}
}

// envStatuses renders just the card status badges (OOB) for live refresh.
func (s *Server) envStatuses(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.loadOrg(w, r)
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
	apps, _ := s.q.ListApplicationsByEnvironment(r.Context(), e.ID)
	ldbs, _ := s.q.ListLogicalDatabasesByEnvironment(r.Context(), e.ID)
	s.deriveCardStatuses(r.Context(), apps, ldbs)
	render(w, r, http.StatusOK, templates.EnvStatuses(apps, ldbs))
}

func projURL(orgID, projID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/projects/" + strconv.FormatInt(projID, 10)
}
