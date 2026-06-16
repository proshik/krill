package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/web/templates"
)

// removeEnvDatabases deletes the managed databases (Swarm service + volume +
// row) of an environment. Used by environment/project delete so their DB
// services and volumes are not orphaned — without this the DB rows cascade-
// delete while the containers and volumes linger with no UI to clean them.
func (s *Server) removeEnvDatabases(ctx context.Context, envID int64) {
	if s.dbsvc == nil {
		return
	}
	pgs, _ := s.q.ListPostgresByEnvironment(ctx, envID)
	for _, pg := range pgs {
		if err := s.dbsvc.DeletePostgres(ctx, pg.ID, true); err != nil {
			slog.Error("removeEnvDatabases: postgres", "db", pg.ID, "err", err)
		}
		_ = s.q.DeleteDBLinksByDB(ctx, db.DeleteDBLinksByDBParams{Engine: "postgres", DbID: pg.ID})
	}
	redises, _ := s.q.ListRedisByEnvironment(ctx, envID)
	for _, rd := range redises {
		if err := s.dbsvc.DeleteRedis(ctx, rd.ID, true); err != nil {
			slog.Error("removeEnvDatabases: redis", "db", rd.ID, "err", err)
		}
		_ = s.q.DeleteDBLinksByDB(ctx, db.DeleteDBLinksByDBParams{Engine: "redis", DbID: rd.ID})
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
		s.flashErr(w, r, "failed to create project: "+err.Error())
		return
	}
	logFrom(r).Info("project created", "project_id", p.ID, "org_id", o.ID, "slug", p.Slug)
	s.setFlash(w, "ok", "Project created")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10), http.StatusSeeOther)
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
		s.flashErr(w, r, "failed to delete project")
		return
	}
	logFrom(r).Info("project deleted", "project_id", p.ID, "org_id", o.ID, "slug", p.Slug)
	s.setFlash(w, "ok", "Project deleted")
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
		s.flashErr(w, r, "failed to create environment: "+err.Error())
		return
	}
	logFrom(r).Info("environment created", "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID, "slug", e.Slug)
	s.setFlash(w, "ok", "Environment created")
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
		s.flashErr(w, r, "failed to delete environment")
		return
	}
	logFrom(r).Info("environment deleted", "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID, "slug", e.Slug)
	s.setFlash(w, "ok", "Environment deleted")
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
	var pgs []db.PostgresDb
	var redises []db.RedisDb
	if active.ID != 0 {
		apps, _ = s.q.ListApplicationsByEnvironment(r.Context(), active.ID)
		pgs, _ = s.q.ListPostgresByEnvironment(r.Context(), active.ID)
		redises, _ = s.q.ListRedisByEnvironment(r.Context(), active.ID)
		// Reflect the LIVE Swarm state on the cards (the stored status can be
		// stale) — one bulk call, not one per card.
		s.deriveCardStatuses(r.Context(), apps, pgs, redises)
	}
	registries, err := s.q.ListRegistriesByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("projectPage: list registries", "err", err, "org_id", o.ID)
	}
	render(w, r, http.StatusOK, templates.Project(o, role, p, envs, active, apps, pgs, redises, registries))
}

// deriveCardStatuses replaces each app/db stored status with the LIVE Swarm
// status using a single bulk ServiceStates call (instead of one per card).
// Engine-nil-safe; unknown services keep their stored status (DeriveStatus).
func (s *Server) deriveCardStatuses(ctx context.Context, apps []db.Application, pgs []db.PostgresDb, redises []db.RedisDb) {
	if s.engine == nil {
		return
	}
	names := make([]string, 0, len(apps)+len(pgs)+len(redises))
	for _, a := range apps {
		names = append(names, dockerName(a.ID))
	}
	for _, p := range pgs {
		names = append(names, p.AppName)
	}
	for _, rd := range redises {
		names = append(names, rd.AppName)
	}
	states, err := s.engine.ServiceStates(ctx, names)
	if err != nil {
		slog.Error("deriveCardStatuses: bulk service states", "err", err)
		return
	}
	for i := range apps {
		apps[i].Status = deploy.DeriveStatus(states[dockerName(apps[i].ID)], apps[i].Status)
	}
	for i := range pgs {
		pgs[i].Status = deploy.DeriveStatus(states[pgs[i].AppName], pgs[i].Status)
	}
	for i := range redises {
		redises[i].Status = deploy.DeriveStatus(states[redises[i].AppName], redises[i].Status)
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
	pgs, _ := s.q.ListPostgresByEnvironment(r.Context(), e.ID)
	redises, _ := s.q.ListRedisByEnvironment(r.Context(), e.ID)
	s.deriveCardStatuses(r.Context(), apps, pgs, redises)
	render(w, r, http.StatusOK, templates.EnvStatuses(apps, pgs, redises))
}

func projURL(orgID, projID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/projects/" + strconv.FormatInt(projID, 10)
}
