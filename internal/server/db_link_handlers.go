package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
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
		s.flashErr(w, r, "invalid database")
		return
	}
	engine := ref[0]
	dbID, err := strconv.ParseInt(ref[1], 10, 64)
	if err != nil {
		s.flashErr(w, r, "invalid database")
		return
	}
	varName := strings.TrimSpace(r.FormValue("var_name"))
	scheme := r.FormValue("scheme")
	if !envVarNameRe.MatchString(varName) {
		s.flashErr(w, r, "invalid variable name (use letters, digits, underscore)")
		return
	}
	switch engine {
	case "postgres":
		if scheme != "postgresql" && scheme != "postgres" {
			s.flashErr(w, r, "invalid scheme for postgres")
			return
		}
	case "redis":
		scheme = "redis"
	default:
		s.flashErr(w, r, "invalid database")
		return
	}
	// the DB must belong to this app's environment
	var envID int64
	if engine == "postgres" {
		pg, gerr := s.q.GetPostgres(r.Context(), dbID)
		if gerr != nil {
			s.flashErr(w, r, "database not found")
			return
		}
		envID = pg.EnvironmentID
	} else {
		rd, gerr := s.q.GetRedis(r.Context(), dbID)
		if gerr != nil {
			s.flashErr(w, r, "database not found")
			return
		}
		envID = rd.EnvironmentID
	}
	if envID != c.Env.ID {
		logFrom(r).Info("addDBLink: db not in app environment", "db_id", dbID, "engine", engine, "app_id", c.App.ID)
		s.flashErr(w, r, "database is not in this environment")
		return
	}
	// var must not already be set in env_text: the link would override it at
	// deploy (see deploy.GetApplication), so reject here to avoid a silent shadow.
	existing, _ := parseEnv(c.App.EnvText)
	if _, dup := existing[varName]; dup {
		s.flashErr(w, r, "variable "+varName+" is already set in the environment; remove it there first")
		return
	}
	if _, err := s.q.CreateDBLink(r.Context(), db.CreateDBLinkParams{
		ApplicationID: c.App.ID, Engine: engine, DbID: dbID, VarName: varName, Scheme: scheme,
	}); err != nil {
		logFrom(r).Error("addDBLink: create failed", "err", err, "app_id", c.App.ID)
		s.flashErr(w, r, "a link for this variable already exists")
		return
	}
	logFrom(r).Info("db link created", "app_id", c.App.ID, "engine", engine, "db_id", dbID, "var", varName)
	s.setFlash(w, "ok", "Database linked — applied on next deploy")
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
		s.flashErr(w, r, "failed to remove link")
		return
	}
	logFrom(r).Info("db link removed", "app_id", c.App.ID, "link_id", l.ID, "var", l.VarName)
	s.setFlash(w, "ok", "Link removed — applied on next deploy")
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}

// loadDBLink parses {linkID} and verifies it belongs to the given application.
func (s *Server) loadDBLink(w http.ResponseWriter, r *http.Request, appID int64) (db.AppDbLink, bool) {
	id, ok := pathID(r, "linkID")
	if !ok {
		http.NotFound(w, r)
		return db.AppDbLink{}, false
	}
	l, err := s.q.GetDBLink(r.Context(), id)
	if err != nil || l.ApplicationID != appID {
		logFrom(r).Info("loadDBLink: not found in app", "link_id", id, "app_id", appID)
		http.NotFound(w, r)
		return db.AppDbLink{}, false
	}
	return l, true
}
