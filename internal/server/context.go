package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
)

// pathID parses a numeric path parameter; returns 0,false if invalid.
func pathID(r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// loadOrg loads the active org (from the middleware context) and the role.
func (s *Server) loadOrg(w http.ResponseWriter, r *http.Request) (db.Organization, string, bool) {
	o, err := s.q.GetOrganization(r.Context(), auth.OrgID(r.Context()))
	if err != nil {
		logFrom(r).Info("organization not found", "org_id", auth.OrgID(r.Context()))
		http.NotFound(w, r)
		return db.Organization{}, "", false
	}
	return o, auth.RoleOf(r.Context()).String(), true
}

// loadProject loads the project, verifying it belongs to the active org.
func (s *Server) loadProject(w http.ResponseWriter, r *http.Request) (db.Project, bool) {
	pid, ok := pathID(r, "projID")
	if !ok {
		http.NotFound(w, r)
		return db.Project{}, false
	}
	p, err := s.org.ProjectInOrg(r.Context(), auth.OrgID(r.Context()), pid)
	if err != nil {
		logFrom(r).Info("project not found in org", "project_id", pid)
		http.NotFound(w, r)
		return db.Project{}, false
	}
	return p, true
}

// loadEnvironment loads the environment, verifying it belongs to the project.
func (s *Server) loadEnvironment(w http.ResponseWriter, r *http.Request, projID int64) (db.Environment, bool) {
	eid, ok := pathID(r, "envID")
	if !ok {
		http.NotFound(w, r)
		return db.Environment{}, false
	}
	e, err := s.org.EnvironmentInProject(r.Context(), projID, eid)
	if err != nil {
		logFrom(r).Info("environment not found in project", "project_id", projID, "environment_id", eid)
		http.NotFound(w, r)
		return db.Environment{}, false
	}
	return e, true
}

// loadApp loads the application, verifying the whole org→proj→env→app chain.
func (s *Server) loadApp(w http.ResponseWriter, r *http.Request, projID, envID int64) (db.Application, bool) {
	aid, ok := pathID(r, "appID")
	if !ok {
		http.NotFound(w, r)
		return db.Application{}, false
	}
	a, err := s.org.AppInChain(r.Context(), auth.OrgID(r.Context()), projID, envID, aid)
	if err != nil {
		logFrom(r).Info("app not found in chain", "project_id", projID, "environment_id", envID, "app_id", aid)
		http.NotFound(w, r)
		return db.Application{}, false
	}
	return a, true
}
