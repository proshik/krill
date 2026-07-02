package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/i18n"
)

var envVarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// addDBLink links a managed DB of the app's environment to an env var (injected
// at deploy). Admin-only.
func (s *Server) addDBLink(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	ref := strings.SplitN(r.FormValue("db_ref"), ":", 2)
	if len(ref) != 2 {
		s.flashErrT(w, r, "flash.err.invalid_database")
		return
	}
	kind := ref[0]
	refID, err := strconv.ParseInt(ref[1], 10, 64)
	if err != nil {
		s.flashErrT(w, r, "flash.err.invalid_database")
		return
	}
	varName := strings.TrimSpace(r.FormValue("var_name"))
	scheme := r.FormValue("scheme")
	if !envVarNameRe.MatchString(varName) {
		s.flashErrT(w, r, "flash.err.invalid_var_name")
		return
	}
	var ldbID, instID *int64
	switch kind {
	case "pg":
		if scheme != "postgresql" && scheme != "postgres" {
			s.flashErrT(w, r, "flash.err.invalid_pg_scheme")
			return
		}
		ld, gerr := s.q.GetLogicalDatabase(r.Context(), refID)
		if gerr != nil || ld.EnvironmentID != c.Env.ID {
			logFrom(r).Info("addDBLink: db not in app environment", "logical_database_id", refID, "app_id", c.App.ID)
			s.flashErrT(w, r, "flash.err.db_not_in_env")
			return
		}
		ldbID = &ld.ID
	case "redis":
		scheme = "redis"
		inst, gerr := s.q.GetDBInstance(r.Context(), refID)
		if gerr != nil || inst.OrganizationID != c.Org.ID || inst.Engine != "redis" {
			s.flashErrT(w, r, "flash.err.db_not_found")
			return
		}
		instID = &inst.ID
	default:
		s.flashErrT(w, r, "flash.err.invalid_database")
		return
	}
	// var must not already be set in env_text: the link would override it at
	// deploy (see deploy.GetApplication), so reject here to avoid a silent shadow.
	existing, _ := parseEnv(c.App.EnvText)
	if _, dup := existing[varName]; dup {
		s.flashErr(w, r, i18n.Tf(r.Context(), "flash.err.var_in_env", varName))
		return
	}
	if _, err := s.q.CreateDBLink(r.Context(), db.CreateDBLinkParams{
		ApplicationID: c.App.ID, LogicalDatabaseID: ldbID, InstanceID: instID, VarName: varName, Scheme: scheme,
	}); err != nil {
		logFrom(r).Error("addDBLink: create failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.link_var_exists")
		return
	}
	logFrom(r).Info("db link created", "app_id", c.App.ID, "kind", kind, "ref_id", refID, "var", varName)
	s.flashOK(w, r, "flash.ok.db_linked")
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}

func (s *Server) deleteDBLink(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	l, ok := s.loadDBLink(w, r, c.App.ID)
	if !ok {
		return
	}
	if err := s.q.DeleteDBLink(r.Context(), l.ID); err != nil {
		logFrom(r).Error("deleteDBLink: delete failed", "err", err, "link_id", l.ID)
		s.flashErrT(w, r, "flash.err.remove_link")
		return
	}
	logFrom(r).Info("db link removed", "app_id", c.App.ID, "link_id", l.ID, "var", l.VarName)
	s.flashOK(w, r, "flash.ok.link_removed")
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}

// loadDBLink parses {linkID} and verifies it belongs to the given application.
func (s *Server) loadDBLink(w http.ResponseWriter, r *http.Request, appID int64) (db.GetDBLinkRow, bool) {
	id, ok := pathID(r, "linkID")
	if !ok {
		http.NotFound(w, r)
		return db.GetDBLinkRow{}, false
	}
	l, err := s.q.GetDBLink(r.Context(), id)
	if err != nil || l.ApplicationID != appID {
		logFrom(r).Info("loadDBLink: not found in app", "link_id", id, "app_id", appID)
		http.NotFound(w, r)
		return db.GetDBLinkRow{}, false
	}
	return l, true
}
