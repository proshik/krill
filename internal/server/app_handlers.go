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

// dockerName — Swarm service name of the application (duplicates docker.ServiceName for brevity).
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
	sourceType := r.FormValue("source_type")
	if sourceType != "dockerfile" {
		sourceType = "image"
	}
	image := strings.TrimSpace(r.FormValue("image"))
	tag := strings.TrimSpace(r.FormValue("tag"))
	if tag == "" {
		tag = "latest"
	}
	gitURL := strings.TrimSpace(r.FormValue("git_url"))
	gitBranch := strings.TrimSpace(r.FormValue("git_branch"))
	if gitBranch == "" {
		gitBranch = "main"
	}
	dockerfilePath := strings.TrimSpace(r.FormValue("dockerfile_path"))
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	domain := strings.TrimSpace(r.FormValue("domain"))
	if domain == "" {
		domain = name + "." + s.cfg.BaseDomain
	}
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if err != nil || port <= 0 || !isSlug(name) {
		logFrom(r).Info("createApp: invalid fields", "environment_id", e.ID, "name", name)
		http.Error(w, "check the fields: name (slug), port", http.StatusBadRequest)
		return
	}
	if sourceType == "image" && image == "" {
		logFrom(r).Info("createApp: image required for image source", "environment_id", e.ID, "name", name)
		http.Error(w, "specify image for the 'image' source", http.StatusBadRequest)
		return
	}
	if sourceType == "dockerfile" && gitURL == "" {
		logFrom(r).Info("createApp: git URL required for dockerfile source", "environment_id", e.ID, "name", name)
		http.Error(w, "specify git URL for the 'Dockerfile' source", http.StatusBadRequest)
		return
	}
	a, err := s.q.CreateApplication(r.Context(), db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: name, Image: image, Tag: tag, Domain: domain, Port: int32(port),
		Env: parseEnv(r.FormValue("env")), SourceType: sourceType,
		GitUrl: gitURL, GitBranch: gitBranch, DockerfilePath: dockerfilePath,
	})
	if err != nil {
		logFrom(r).Error("createApp: failed to create application", "err", err, "environment_id", e.ID, "name", name)
		http.Error(w, "failed to create (name/domain already taken?): "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.q.CreateDomain(r.Context(), db.CreateDomainParams{
		ApplicationID: a.ID, Host: a.Domain, Tls: false, IsPrimary: true,
	}); err != nil {
		logFrom(r).Error("createApp: create primary domain failed", "err", err, "app_id", a.ID, "host", a.Domain)
		http.Error(w, "failed to create domain", http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("application created", "app_id", a.ID, "environment_id", e.ID, "name", name)
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab != "env" && tab != "logs" && tab != "deployments" && tab != "domains" {
		tab = "general"
	}
	if tab == "deployments" {
		deps, err := s.q.ListDeploymentsByApplication(r.Context(), c.App.ID)
		if err != nil {
			logFrom(r).Error("appDetail: failed to list deployments", "err", err, "app_id", c.App.ID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.Deps = deps
	}
	if tab == "domains" {
		doms, err := s.q.ListDomainsByApplication(r.Context(), c.App.ID)
		if err != nil {
			logFrom(r).Error("appDetail: failed to list domains", "err", err, "app_id", c.App.ID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.Domains = doms
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
		} else {
			logFrom(r).Error("appStatus: failed to query engine service state", "err", err, "app_id", c.App.ID)
		}
	}
	render(w, r, http.StatusOK, templates.StatusBadge(status))
}

func (s *Server) deployApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if c.App.SourceType == "dockerfile" {
		gitURL := strings.TrimSpace(r.FormValue("git_url"))
		gitBranch := strings.TrimSpace(r.FormValue("git_branch"))
		dockerfilePath := strings.TrimSpace(r.FormValue("dockerfile_path"))
		if gitURL != "" {
			if err := s.q.UpdateApplicationSource(r.Context(), db.UpdateApplicationSourceParams{
				ID: c.App.ID, GitUrl: gitURL, GitBranch: gitBranch, DockerfilePath: dockerfilePath,
			}); err != nil {
				logFrom(r).Error("deployApp: failed to update application source", "err", err, "app_id", c.App.ID)
				http.Error(w, "failed to update source", http.StatusInternalServerError)
				return
			}
		}
	} else {
		image := strings.TrimSpace(r.FormValue("image"))
		tag := strings.TrimSpace(r.FormValue("tag"))
		if image != "" && tag != "" {
			if err := s.q.UpdateApplicationImage(r.Context(), db.UpdateApplicationImageParams{ID: c.App.ID, Image: image, Tag: tag}); err != nil {
				logFrom(r).Error("deployApp: failed to update application image", "err", err, "app_id", c.App.ID, "image", image)
				http.Error(w, "failed to update image", http.StatusInternalServerError)
				return
			}
		}
	}
	s.deployer.Enqueue(c.App.ID, "manual")
	logFrom(r).Info("deploy enqueued", "app_id", c.App.ID, "app_name", c.App.Name)
	http.Redirect(w, r, appURL(c)+"?tab=deployments", http.StatusSeeOther)
}

func (s *Server) saveEnv(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if err := s.q.UpdateApplicationEnv(r.Context(), db.UpdateApplicationEnvParams{
		ID: c.App.ID, Env: parseEnv(r.FormValue("env")),
	}); err != nil {
		logFrom(r).Error("saveEnv: failed to update application env", "err", err, "app_id", c.App.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("environment variables updated", "app_id", c.App.ID, "app_name", c.App.Name)
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	_ = s.engine.ServiceRemove(r.Context(), dockerName(c.App.ID))
	if err := s.q.DeleteApplication(r.Context(), c.App.ID); err != nil {
		logFrom(r).Error("deleteApp: failed to delete application", "err", err, "app_id", c.App.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("application deleted", "app_id", c.App.ID, "app_name", c.App.Name)
	http.Redirect(w, r, envURL(c.Org.ID, c.Project.ID, c.Env.ID), http.StatusSeeOther)
}

// loadAppCtx loads the full org→proj→env→app chain for rendering/URLs.
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
