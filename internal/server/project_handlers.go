package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
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
	}
	redises, _ := s.q.ListRedisByEnvironment(ctx, envID)
	for _, rd := range redises {
		if err := s.dbsvc.DeleteRedis(ctx, rd.ID, true); err != nil {
			slog.Error("removeEnvDatabases: redis", "db", rd.ID, "err", err)
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
		// Reflect the LIVE Swarm state on the cards: the stored status can be
		// stale (e.g. a slow deploy was marked error but the service is healthy).
		if s.engine != nil {
			for i := range apps {
				if st, err := s.engine.ServiceState(r.Context(), dockerName(apps[i].ID)); err == nil {
					apps[i].Status = deploy.DeriveStatus(st, apps[i].Status)
				}
			}
			for i := range pgs {
				if st, err := s.engine.ServiceState(r.Context(), pgs[i].AppName); err == nil {
					pgs[i].Status = deploy.DeriveStatus(st, pgs[i].Status)
				}
			}
			for i := range redises {
				if st, err := s.engine.ServiceState(r.Context(), redises[i].AppName); err == nil {
					redises[i].Status = deploy.DeriveStatus(st, redises[i].Status)
				}
			}
		}
	}
	registries, err := s.q.ListRegistriesByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("projectPage: list registries", "err", err, "org_id", o.ID)
	}
	render(w, r, http.StatusOK, templates.Project(o, role, p, envs, active, apps, pgs, redises, registries))
}

func projURL(orgID, projID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/projects/" + strconv.FormatInt(projID, 10)
}
