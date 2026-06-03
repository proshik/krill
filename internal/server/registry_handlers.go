package server

import (
	"net/http"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// listRegistries renders the org's private container registries (readable by
// any member).
func (s *Server) listRegistries(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	regs, err := s.q.ListRegistriesByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("listRegistries: failed to list registries", "err", err, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.Registries(o, role, regs))
}

// createRegistry validates the form, optionally checks credentials, and
// persists a new registry (admin-only). The password/token is never logged.
func (s *Server) createRegistry(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	name := r.FormValue("name")
	registryURL := r.FormValue("registry_url")
	username := r.FormValue("username")
	password := r.FormValue("password")

	if name == "" || registryURL == "" || username == "" || password == "" {
		http.Error(w, "name, registry_url, username and password are required", http.StatusBadRequest)
		return
	}

	n, err := s.q.CountRegistriesByName(r.Context(), db.CountRegistriesByNameParams{OrganizationID: o.ID, Name: name})
	if err != nil {
		logFrom(r).Error("createRegistry: failed to count registries", "err", err, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n > 0 {
		http.Error(w, "a registry with this name already exists", http.StatusBadRequest)
		return
	}

	if s.engine != nil {
		if err := s.engine.RegistryCheck(r.Context(), registryURL, username, password); err != nil {
			logFrom(r).Info("createRegistry: auth check failed", "org_id", o.ID, "registry_url", registryURL)
			http.Error(w, "cannot authenticate to registry", http.StatusBadRequest)
			return
		}
	}

	reg, err := s.q.CreateRegistry(r.Context(), db.CreateRegistryParams{
		OrganizationID: o.ID,
		Name:           name,
		RegistryUrl:    registryURL,
		Username:       username,
		Password:       password,
	})
	if err != nil {
		logFrom(r).Error("createRegistry: failed to create registry", "err", err, "org_id", o.ID, "name", name)
		http.Error(w, "failed to create registry: "+err.Error(), http.StatusBadRequest)
		return
	}
	logFrom(r).Info("registry created", "org_id", o.ID, "registry_id", reg.ID, "name", reg.Name, "registry_url", reg.RegistryUrl, "username", reg.Username)
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/registries", http.StatusSeeOther)
}

// deleteRegistry removes a registry (admin-only). Deletion is blocked while
// applications still reference it.
func (s *Server) deleteRegistry(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r, "regID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	reg, err := s.q.GetRegistry(r.Context(), id)
	if err != nil || reg.OrganizationID != o.ID {
		logFrom(r).Info("deleteRegistry: registry not found or org mismatch", "registry_id", id, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	id2 := id
	if n, _ := s.q.CountApplicationsByRegistry(r.Context(), &id2); n > 0 {
		http.Error(w, "registry is used by an application", http.StatusBadRequest)
		return
	}
	if err := s.q.DeleteRegistry(r.Context(), id); err != nil {
		logFrom(r).Error("deleteRegistry: failed to delete registry", "err", err, "registry_id", id, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("registry deleted", "org_id", o.ID, "registry_id", id)
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/registries", http.StatusSeeOther)
}
