package server

import (
	"log/slog"
	"net/http"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, err := s.org.CreateProject(r.Context(), o.ID, r.FormValue("name"), r.FormValue("description"))
	if err != nil {
		logFrom(r).Info("createProject: rejected", "err", err, "org_id", o.ID)
		http.Error(w, "failed to create project: "+err.Error(), http.StatusBadRequest)
		return
	}
	logFrom(r).Info("project created", "project_id", p.ID, "org_id", o.ID, "slug", p.Slug)
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
	}
	if err := s.q.DeleteProject(r.Context(), p.ID); err != nil {
		logFrom(r).Error("deleteProject: delete project", "err", err, "project_id", p.ID, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("project deleted", "project_id", p.ID, "org_id", o.ID, "slug", p.Slug)
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
		http.Error(w, "failed to create environment: "+err.Error(), http.StatusBadRequest)
		return
	}
	logFrom(r).Info("environment created", "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID, "slug", e.Slug)
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
	if err := s.q.DeleteEnvironment(r.Context(), e.ID); err != nil {
		logFrom(r).Error("deleteEnvironment: delete environment", "err", err, "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("environment deleted", "environment_id", e.ID, "project_id", p.ID, "org_id", o.ID, "slug", e.Slug)
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
