package server

import (
	"net/http"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/web/templates"
)

// dockerName — имя Swarm-сервиса приложения (дублирует docker.ServiceName для краткости).
func dockerName(appID int64) string { return docker.ServiceName(appID) }

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
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
	name := strings.TrimSpace(r.FormValue("name"))
	image := strings.TrimSpace(r.FormValue("image"))
	tag := strings.TrimSpace(r.FormValue("tag"))
	if tag == "" {
		tag = "latest"
	}
	domain := strings.TrimSpace(r.FormValue("domain"))
	if domain == "" {
		domain = name + "." + s.cfg.BaseDomain
	}
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if err != nil || port <= 0 || !isSlug(name) || image == "" {
		http.Error(w, "проверь поля: имя (slug), образ, порт", http.StatusBadRequest)
		return
	}
	if _, err := s.q.CreateApplication(r.Context(), db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: name, Image: image, Tag: tag,
		Domain: domain, Port: int32(port), Env: parseEnv(r.FormValue("env")),
	}); err != nil {
		http.Error(w, "не удалось создать (имя/домен занят?): "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab != "env" && tab != "logs" {
		tab = "general"
	}
	render(w, r, http.StatusOK, templates.AppDetail(c, tab))
}

func (s *Server) appStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	status := c.App.Status
	if s.engine != nil {
		if st, err := s.engine.ServiceState(r.Context(), dockerName(c.App.ID)); err == nil {
			status = deploy.DeriveStatus(st, c.App.Status)
		}
	}
	render(w, r, http.StatusOK, templates.StatusBadge(status))
}

func (s *Server) deployApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	image := strings.TrimSpace(r.FormValue("image"))
	tag := strings.TrimSpace(r.FormValue("tag"))
	if image != "" && tag != "" {
		_ = s.q.UpdateApplicationImage(r.Context(), db.UpdateApplicationImageParams{ID: c.App.ID, Image: image, Tag: tag})
	}
	s.deployer.Enqueue(c.App.ID)
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

func (s *Server) saveEnv(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if err := s.q.UpdateApplicationEnv(r.Context(), db.UpdateApplicationEnvParams{
		ID: c.App.ID, Env: parseEnv(r.FormValue("env")),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	_ = s.engine.ServiceRemove(r.Context(), dockerName(c.App.ID))
	if err := s.q.DeleteApplication(r.Context(), c.App.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, envURL(c.Org.ID, c.Project.ID, c.Env.ID), http.StatusSeeOther)
}

// loadAppCtx грузит полную цепочку org→proj→env→app для рендера/URL.
func (s *Server) loadAppCtx(w http.ResponseWriter, r *http.Request) (templates.AppCtx, bool) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return templates.AppCtx{}, false
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return templates.AppCtx{}, false
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return templates.AppCtx{}, false
	}
	a, ok := s.loadApp(w, r, p.ID, e.ID)
	if !ok {
		return templates.AppCtx{}, false
	}
	return templates.AppCtx{Org: o, Role: role, Project: p, Env: e, App: a}, true
}

func envURL(orgID, projID, envID int64) string {
	return projURL(orgID, projID) + "/environments/" + strconv.FormatInt(envID, 10)
}

func appURL(c templates.AppCtx) string {
	return envURL(c.Org.ID, c.Project.ID, c.Env.ID) + "/apps/" + strconv.FormatInt(c.App.ID, 10)
}

func isSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return false
		}
	}
	return true
}

func parseEnv(raw string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return env
}
