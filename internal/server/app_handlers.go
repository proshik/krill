package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/web/templates"
)

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.q.ListApplications(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, templates.Apps(apps))
}

func (s *Server) newApp(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, templates.AppForm("", s.cfg.BaseDomain))
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
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
		render(w, r, http.StatusBadRequest, templates.AppForm("Проверь поля: имя (slug), образ, порт", s.cfg.BaseDomain))
		return
	}

	_, err = s.q.CreateApplication(r.Context(), db.CreateApplicationParams{
		Name:   name,
		Image:  image,
		Tag:    tag,
		Domain: domain,
		Port:   int32(port),
		Env:    parseEnv(r.FormValue("env")),
	})
	if err != nil {
		render(w, r, http.StatusBadRequest, templates.AppForm("Не удалось создать (имя занято?): "+err.Error(), s.cfg.BaseDomain))
		return
	}
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab != "logs" {
		tab = "general"
	}
	render(w, r, http.StatusOK, templates.AppDetail(app, tab))
}

func (s *Server) appStatus(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	status := app.Status
	if s.engine != nil {
		if st, err := s.engine.ServiceState(r.Context(), docker.ServiceName(app.Name)); err == nil {
			status = deploy.DeriveStatus(st, app.Status)
		}
	}
	render(w, r, http.StatusOK, templates.StatusBadge(status))
}

func (s *Server) deployApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	image := strings.TrimSpace(r.FormValue("image"))
	tag := strings.TrimSpace(r.FormValue("tag"))
	if image != "" && tag != "" {
		_ = s.q.UpdateApplicationImage(r.Context(), db.UpdateApplicationImageParams{ID: app.ID, Image: image, Tag: tag})
	}
	s.deployer.Enqueue(app.ID)
	http.Redirect(w, r, "/apps/"+strconv.FormatInt(app.ID, 10), http.StatusSeeOther)
}

// --- helpers ---

func (s *Server) loadApp(w http.ResponseWriter, r *http.Request) (db.Application, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return db.Application{}, false
	}
	app, err := s.q.GetApplication(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return db.Application{}, false
	}
	return app, true
}

func isSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
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
