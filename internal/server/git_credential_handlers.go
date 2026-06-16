package server

import (
	"net/http"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/templates"
)

// listGitCredentials renders the org's git credentials (readable by any member).
func (s *Server) listGitCredentials(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	creds, err := s.q.ListGitCredentialsByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("listGitCredentials: failed to list", "err", err, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.GitCredentials(o, role, creds))
}

// createGitCredential validates the form and persists a new git credential
// (admin-only). The token is encrypted at rest and never logged.
func (s *Server) createGitCredential(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	host := strings.TrimSpace(r.FormValue("host"))
	username := strings.TrimSpace(r.FormValue("username"))
	token := r.FormValue("token")
	if name == "" || host == "" || username == "" || token == "" {
		s.flashErrT(w, r, "flash.err.gitcred_fields_required")
		return
	}
	n, err := s.q.CountGitCredentialsByName(r.Context(), db.CountGitCredentialsByNameParams{OrganizationID: o.ID, Name: name})
	if err != nil {
		logFrom(r).Error("createGitCredential: count failed", "err", err, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	if n > 0 {
		s.flashErrT(w, r, "flash.err.gitcred_name_exists")
		return
	}
	gc, err := s.q.CreateGitCredential(r.Context(), db.CreateGitCredentialParams{
		OrganizationID: o.ID, Name: name, Host: host, Username: username, Token: secret.Enc(token),
	})
	if err != nil {
		logFrom(r).Error("createGitCredential: create failed", "err", err, "org_id", o.ID, "name", name)
		s.flashErrErr(w, r, "flash.err.create_gitcred", err)
		return
	}
	logFrom(r).Info("git credential created", "org_id", o.ID, "git_credential_id", gc.ID, "name", gc.Name, "host", gc.Host, "username", gc.Username)
	s.flashOK(w, r, "flash.ok.gitcred_created")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/git-credentials", http.StatusSeeOther)
}

// deleteGitCredential removes a git credential (admin-only). Deletion is blocked
// while applications still reference it.
func (s *Server) deleteGitCredential(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r, "gcID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	gc, err := s.q.GetGitCredential(r.Context(), id)
	if err != nil || gc.OrganizationID != o.ID {
		logFrom(r).Info("deleteGitCredential: not found or org mismatch", "git_credential_id", id, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	gcID := id
	if n, _ := s.q.CountApplicationsByGitCredential(r.Context(), &gcID); n > 0 {
		s.flashErrT(w, r, "flash.err.gitcred_in_use")
		return
	}
	if err := s.q.DeleteGitCredential(r.Context(), id); err != nil {
		logFrom(r).Error("deleteGitCredential: delete failed", "err", err, "git_credential_id", id, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("git credential deleted", "org_id", o.ID, "git_credential_id", id)
	s.flashOK(w, r, "flash.ok.gitcred_deleted")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/git-credentials", http.StatusSeeOther)
}
