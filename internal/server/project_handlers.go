package server

import (
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
	if _, err := s.org.CreateProject(r.Context(), o.ID, r.FormValue("name"), r.FormValue("description")); err != nil {
		http.Error(w, "не удалось создать проект: "+err.Error(), http.StatusBadRequest)
		return
	}
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
	// снять Swarm-сервисы всех приложений проекта
	envs, _ := s.q.ListEnvironments(r.Context(), p.ID)
	for _, e := range envs {
		apps, _ := s.q.ListApplicationsByEnvironment(r.Context(), e.ID)
		for _, a := range apps {
			_ = s.engine.ServiceRemove(r.Context(), dockerName(a.ID))
		}
	}
	if err := s.q.DeleteProject(r.Context(), p.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
	if _, err := s.org.CreateEnvironment(r.Context(), p.ID, r.FormValue("name")); err != nil {
		http.Error(w, "не удалось создать окружение: "+err.Error(), http.StatusBadRequest)
		return
	}
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
	apps, _ := s.q.ListApplicationsByEnvironment(r.Context(), e.ID)
	for _, a := range apps {
		_ = s.engine.ServiceRemove(r.Context(), dockerName(a.ID))
	}
	if err := s.q.DeleteEnvironment(r.Context(), e.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, projURL(o.ID, p.ID), http.StatusSeeOther)
}

// projectPage отображает проект с выбранным (или первым) окружением.
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
	if active.ID != 0 {
		apps, _ = s.q.ListApplicationsByEnvironment(r.Context(), active.ID)
	}
	render(w, r, http.StatusOK, templates.Project(o, role, p, envs, active, apps))
}

func projURL(orgID, projID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/projects/" + strconv.FormatInt(projID, 10)
}
