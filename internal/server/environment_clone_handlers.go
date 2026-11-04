package server

import (
	"errors"
	"net/http"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/envclone"
)

func (s *Server) SetEnvironmentCloner(cloner *envclone.Service) { s.cloner = cloner }

func (s *Server) cloneEnvironment(w http.ResponseWriter, r *http.Request) {
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
	if s.cloner == nil {
		s.flashErrT(w, r, "clone.unavailable")
		return
	}
	target, err := s.cloner.Clone(r.Context(), envclone.Request{Key: r.FormValue("request_key"), Name: r.FormValue("name"), UserID: auth.UserID(r.Context()), OrgID: o.ID, ProjectID: p.ID, SourceID: e.ID})
	if err != nil {
		if errors.Is(err, envclone.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		key := "clone.failed"
		switch {
		case errors.Is(err, dbservice.ErrCloneIsolation):
			key = "clone.isolation_required"
		case errors.Is(err, envclone.ErrNameConflict):
			key = "clone.name_conflict"
		case errors.Is(err, envclone.ErrBusy):
			key = "clone.busy"
		case errors.Is(err, envclone.ErrCleanupPending):
			key = "clone.cleanup_pending"
		case errors.Is(err, envclone.ErrOutcomeUnknown):
			key = "clone.outcome_unknown"
		case errors.Is(err, envclone.ErrUnavailable):
			key = "clone.unavailable"
		}
		// Raw PG/exec errors may carry secrets. Log only the safe category and IDs.
		logFrom(r).Warn("environment clone refused", "source_id", e.ID, "reason", key)
		s.flashErrT(w, r, key)
		return
	}
	logFrom(r).Info("environment cloned", "source_id", e.ID, "environment_id", target)
	s.flashOK(w, r, "clone.created")
	http.Redirect(w, r, envURL(o.ID, p.ID, target), http.StatusSeeOther)
}
