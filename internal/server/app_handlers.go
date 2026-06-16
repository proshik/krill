package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
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
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	if domain == "" {
		domain = name + "." + s.cfg.BaseDomain
	}
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if err != nil || port <= 0 || !isSlug(name) {
		logFrom(r).Info("createApp: invalid fields", "environment_id", e.ID, "name", name)
		s.flashErr(w, r, "check the fields: name (slug), port")
		return
	}
	if !validHost(domain) {
		logFrom(r).Info("createApp: invalid domain", "environment_id", e.ID, "name", name, "domain", domain)
		s.flashErr(w, r, "invalid domain")
		return
	}
	if n, _ := s.q.CountDomainsByHost(r.Context(), domain); n > 0 {
		logFrom(r).Info("createApp: domain already in use", "environment_id", e.ID, "name", name, "domain", domain)
		s.flashErr(w, r, "domain already in use")
		return
	}
	if sourceType == "image" && image == "" {
		logFrom(r).Info("createApp: image required for image source", "environment_id", e.ID, "name", name)
		s.flashErr(w, r, "specify image for the 'image' source")
		return
	}
	if sourceType == "dockerfile" && gitURL == "" {
		logFrom(r).Info("createApp: git URL required for dockerfile source", "environment_id", e.ID, "name", name)
		s.flashErr(w, r, "specify git URL for the 'Dockerfile' source")
		return
	}
	var registryID *int64
	if v := strings.TrimSpace(r.FormValue("registry_id")); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			s.flashErr(w, r, "invalid registry")
			return
		}
		reg, gerr := s.q.GetRegistry(r.Context(), n)
		if gerr != nil || reg.OrganizationID != o.ID {
			logFrom(r).Info("createApp: registry not found or org mismatch", "registry_id", n, "org_id", o.ID)
			s.flashErr(w, r, "invalid registry")
			return
		}
		registryID = &n
	}
	a, err := s.q.CreateApplication(r.Context(), db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: name, Image: image, Tag: tag, Domain: domain, Port: int32(port),
		EnvText: r.FormValue("env"), SourceType: sourceType,
		GitUrl: gitURL, GitBranch: gitBranch, DockerfilePath: dockerfilePath,
	})
	if err != nil {
		logFrom(r).Error("createApp: failed to create application", "err", err, "environment_id", e.ID, "name", name)
		s.flashErr(w, r, "failed to create (name/domain already taken?): "+err.Error())
		return
	}
	if _, err := s.q.CreateDomain(r.Context(), db.CreateDomainParams{
		ApplicationID: a.ID, Host: a.Domain, Tls: false, IsPrimary: true, Exposed: false, Paths: "",
	}); err != nil {
		// Roll back the orphaned application so it does not linger without a domain.
		if derr := s.q.DeleteApplication(r.Context(), a.ID); derr != nil {
			logFrom(r).Error("createApp: rollback delete application failed", "err", derr, "app_id", a.ID)
		}
		logFrom(r).Error("createApp: create primary domain failed", "err", err, "app_id", a.ID, "host", a.Domain)
		s.flashErr(w, r, "failed to create domain")
		return
	}
	if registryID != nil {
		if err := s.q.SetApplicationRegistry(r.Context(), db.SetApplicationRegistryParams{ID: a.ID, RegistryID: registryID}); err != nil {
			logFrom(r).Error("createApp: set registry failed", "err", err, "app_id", a.ID)
		}
	}
	logFrom(r).Info("application created", "app_id", a.ID, "environment_id", e.ID, "name", name)
	s.setFlash(w, "ok", "Application created")
	http.Redirect(w, r, envURL(o.ID, p.ID, e.ID), http.StatusSeeOther)
}

func (s *Server) setAppRegistry(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	o, _, _ := s.loadOrg(w, r)
	var registryID *int64
	if v := strings.TrimSpace(r.FormValue("registry_id")); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			s.flashErr(w, r, "invalid registry")
			return
		}
		reg, gerr := s.q.GetRegistry(r.Context(), n)
		if gerr != nil || reg.OrganizationID != o.ID {
			s.flashErr(w, r, "invalid registry")
			return
		}
		registryID = &n
	}
	if err := s.q.SetApplicationRegistry(r.Context(), db.SetApplicationRegistryParams{ID: c.App.ID, RegistryID: registryID}); err != nil {
		logFrom(r).Error("setAppRegistry: update failed", "err", err, "app_id", c.App.ID)
		s.flashErr(w, r, "failed to set registry")
		return
	}
	logFrom(r).Info("application registry set", "app_id", c.App.ID, "registry_id", registryID)
	s.setFlash(w, "ok", "Registry updated")
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab != "env" && tab != "logs" && tab != "deployments" && tab != "domains" && tab != "advanced" && tab != "volumes" && tab != "terminal" {
		tab = "general"
	}
	// Env values are secrets. Members are read-only viewers and must never see
	// them (mirrors the admin-only DB connection-string display), so the env tab
	// silently falls back to General for non-admins.
	if (tab == "env" || tab == "volumes") && c.Role != "owner" && c.Role != "admin" {
		tab = "general"
	}
	if regs, err := s.q.ListRegistriesByOrg(r.Context(), c.Org.ID); err != nil {
		logFrom(r).Error("appDetail: failed to list registries", "err", err, "org_id", c.Org.ID)
	} else {
		c.Registries = regs
	}
	if n, err := s.q.CountExposedDomainsByApplication(r.Context(), c.App.ID); err != nil {
		logFrom(r).Error("appDetail: count exposed domains", "err", err, "app_id", c.App.ID)
	} else {
		c.Exposed = n > 0
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
	if tab == "env" {
		links, lerr := s.q.ListDBLinksByApplication(r.Context(), c.App.ID)
		if lerr != nil {
			logFrom(r).Error("appDetail: list db links", "err", lerr, "app_id", c.App.ID)
		}
		pgs, _ := s.q.ListPostgresByEnvironment(r.Context(), c.Env.ID)
		redises, _ := s.q.ListRedisByEnvironment(r.Context(), c.Env.ID)
		pgName := map[int64]string{}
		for _, pg := range pgs {
			pgName[pg.ID] = pg.Name
			c.EnvDatabases = append(c.EnvDatabases, templates.EnvDBOption{Engine: "postgres", ID: pg.ID, Name: pg.Name})
		}
		redisName := map[int64]string{}
		for _, rd := range redises {
			redisName[rd.ID] = rd.Name
			c.EnvDatabases = append(c.EnvDatabases, templates.EnvDBOption{Engine: "redis", ID: rd.ID, Name: rd.Name})
		}
		envKeys, _ := parseEnv(c.App.EnvText)
		for _, l := range links {
			name := pgName[l.DbID]
			if l.Engine == "redis" {
				name = redisName[l.DbID]
			}
			_, collides := envKeys[l.VarName]
			c.DBLinks = append(c.DBLinks, templates.DBLinkView{
				ID: l.ID, Engine: l.Engine, DBName: name, VarName: l.VarName, Scheme: l.Scheme, Collides: collides,
			})
		}
	}
	if tab == "volumes" {
		vols, err := s.q.ListVolumesByApplication(r.Context(), c.App.ID)
		if err != nil {
			logFrom(r).Error("appDetail: failed to list volumes", "err", err, "app_id", c.App.ID)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.Volumes = vols
		if dests, derr := s.q.ListDestinationsByOrg(r.Context(), c.Org.ID); derr != nil {
			logFrom(r).Error("appDetail: failed to list destinations", "err", derr, "org_id", c.Org.ID)
		} else {
			c.Destinations = dests
		}
		c.VolumeBackups = map[int64][]db.VolumeBackup{}
		for _, v := range vols {
			if bks, berr := s.q.ListVolumeBackupsByVolume(r.Context(), v.ID); berr != nil {
				logFrom(r).Error("appDetail: failed to list volume backups", "err", berr, "volume_id", v.ID)
			} else {
				c.VolumeBackups[v.ID] = bks
			}
		}
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

func (s *Server) appTags(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if c.App.RegistryID == nil {
		http.Error(w, "select a registry first", http.StatusBadRequest)
		return
	}
	reg, err := s.q.GetRegistry(r.Context(), *c.App.RegistryID)
	if err != nil {
		http.Error(w, "registry not found", http.StatusBadRequest)
		return
	}
	repo := docker.RegistryRepo(reg.RegistryUrl, c.App.Image)
	tags, err := docker.RegistryListTags(r.Context(), reg.RegistryUrl, reg.Username, secret.Dec(reg.Password), repo)
	if err != nil {
		logFrom(r).Info("appTags: list tags failed", "err", err, "app_id", c.App.ID, "image", c.App.Image)
		// Return 200 with a visible message: htmx does not swap on 4xx/5xx, so the
		// button would otherwise look dead. Keep the manual tag input usable.
		render(w, r, http.StatusOK, templates.TagError(c.App.Tag,
			"Could not load tags. The image must be a full ref (e.g. ghcr.io/owner/name) and the registry must have valid credentials."))
		return
	}
	render(w, r, http.StatusOK, templates.TagOptions(tags, c.App.Tag))
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
				s.flashErr(w, r, "failed to update source")
				return
			}
		}
	} else {
		image := strings.TrimSpace(r.FormValue("image"))
		tag := strings.TrimSpace(r.FormValue("tag"))
		if image != "" && tag != "" {
			if err := s.q.UpdateApplicationImage(r.Context(), db.UpdateApplicationImageParams{ID: c.App.ID, Image: image, Tag: tag}); err != nil {
				logFrom(r).Error("deployApp: failed to update application image", "err", err, "app_id", c.App.ID, "image", image)
				s.flashErr(w, r, "failed to update image")
				return
			}
		}
	}
	if s.deployer.Enqueue(c.App.ID, "manual") == 0 {
		logFrom(r).Error("deployApp: deployment not accepted (queue full or shutting down)", "app_id", c.App.ID)
		s.flashErr(w, r, "could not queue the deployment — try again later")
		return
	}
	logFrom(r).Info("deploy enqueued", "app_id", c.App.ID, "app_name", c.App.Name)
	s.setFlash(w, "ok", "Deployment queued")
	http.Redirect(w, r, appURL(c)+"?tab=deployments", http.StatusSeeOther)
}

// rebuildApp enqueues a from-scratch deployment: docker build --no-cache for
// Dockerfile apps, re-pull for image apps.
func (s *Server) rebuildApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if s.deployer.EnqueueRebuild(c.App.ID, "manual") == 0 {
		logFrom(r).Error("rebuildApp: rebuild not accepted (queue full or shutting down)", "app_id", c.App.ID)
		s.flashErr(w, r, "could not queue the rebuild — try again later")
		return
	}
	logFrom(r).Info("rebuild enqueued", "app_id", c.App.ID, "app_name", c.App.Name)
	s.setFlash(w, "ok", "Rebuild queued (no cache)")
	http.Redirect(w, r, appURL(c)+"?tab=deployments", http.StatusSeeOther)
}

// reloadApp force-restarts the running containers without rebuilding/re-pulling.
func (s *Server) reloadApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if s.engine != nil {
		if err := s.engine.ServiceRestart(r.Context(), dockerName(c.App.ID)); err != nil {
			logFrom(r).Error("reloadApp: failed to restart service", "err", err, "app_id", c.App.ID)
			s.flashErr(w, r, "failed to reload application")
			return
		}
	}
	logFrom(r).Info("application reloaded", "app_id", c.App.ID, "app_name", c.App.Name)
	s.setFlash(w, "ok", "Reload requested")
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

// stopApp scales the app's service to zero replicas and marks it idle.
func (s *Server) stopApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if s.engine != nil {
		if err := s.engine.ServiceScale(r.Context(), dockerName(c.App.ID), 0); err != nil {
			logFrom(r).Error("stopApp: failed to scale to zero", "err", err, "app_id", c.App.ID)
			s.flashErr(w, r, "failed to stop application")
			return
		}
	}
	if err := s.q.UpdateApplicationStatus(r.Context(), db.UpdateApplicationStatusParams{ID: c.App.ID, Status: deploy.StatusIdle}); err != nil {
		logFrom(r).Error("stopApp: failed to update status", "err", err, "app_id", c.App.ID)
	}
	logFrom(r).Info("application stopped", "app_id", c.App.ID, "app_name", c.App.Name)
	s.setFlash(w, "ok", "Application stopped")
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

func (s *Server) saveEnv(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	raw := r.FormValue("env")
	if _, dups := parseEnv(raw); len(dups) > 0 {
		s.flashErr(w, r, "duplicate variable: "+dups[0])
		return
	}
	if err := s.q.UpdateApplicationEnv(r.Context(), db.UpdateApplicationEnvParams{
		ID: c.App.ID, EnvText: raw,
	}); err != nil {
		logFrom(r).Error("saveEnv: failed to update application env", "err", err, "app_id", c.App.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("environment variables updated", "app_id", c.App.ID, "app_name", c.App.Name)
	s.setFlash(w, "ok", "Variables saved — applied on next deploy")
	http.Redirect(w, r, appURL(c)+"?tab=env", http.StatusSeeOther)
}

// saveAdvanced validates and persists the app's advanced container settings.
// Applied on the next deploy (the ServiceSpec is rebuilt from these values).
func (s *Server) saveAdvanced(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	command := strings.TrimSpace(r.FormValue("command"))
	memLimit := strings.TrimSpace(r.FormValue("memory_limit"))
	if _, err := docker.ParseMemoryBytes(memLimit); err != nil {
		s.flashErr(w, r, "invalid memory limit (e.g. 256m, 1g)")
		return
	}
	cpuLimit := strings.TrimSpace(r.FormValue("cpu_limit"))
	if _, err := docker.ParseNanoCPUs(cpuLimit); err != nil {
		s.flashErr(w, r, "invalid CPU limit (e.g. 0.5, 1)")
		return
	}
	replicas, err := strconv.Atoi(strings.TrimSpace(r.FormValue("replicas")))
	if err != nil || replicas < 1 || replicas > 20 {
		s.flashErr(w, r, "replicas must be between 1 and 20")
		return
	}
	cond := r.FormValue("restart_condition")
	if cond != "any" && cond != "on-failure" && cond != "none" {
		s.flashErr(w, r, "invalid restart condition")
		return
	}
	maxAttStr := strings.TrimSpace(r.FormValue("restart_max_attempts"))
	if maxAttStr == "" {
		maxAttStr = "0"
	}
	maxAtt, err := strconv.Atoi(maxAttStr)
	if err != nil || maxAtt < 0 {
		s.flashErr(w, r, "max restart attempts must be 0 or more")
		return
	}
	hcCmd := strings.TrimSpace(r.FormValue("healthcheck_cmd"))
	hcInterval := strings.TrimSpace(r.FormValue("healthcheck_interval"))
	hcTimeout := strings.TrimSpace(r.FormValue("healthcheck_timeout"))
	hcStart := strings.TrimSpace(r.FormValue("healthcheck_start_period"))
	hcRetries := strings.TrimSpace(r.FormValue("healthcheck_retries"))
	if hcCmd == "" {
		hcInterval, hcTimeout, hcStart, hcRetries = "", "", "", ""
	} else {
		for _, d := range []string{hcInterval, hcTimeout, hcStart} {
			if d == "" {
				continue
			}
			if _, perr := time.ParseDuration(d); perr != nil {
				s.flashErr(w, r, "invalid healthcheck duration (e.g. 30s, 1m)")
				return
			}
		}
		if hcRetries != "" {
			if n, perr := strconv.Atoi(hcRetries); perr != nil || n < 0 {
				s.flashErr(w, r, "healthcheck retries must be 0 or more")
				return
			}
		}
	}

	if err := s.q.UpdateApplicationAdvanced(r.Context(), db.UpdateApplicationAdvancedParams{
		ID:                     c.App.ID,
		Command:                nilIfEmpty(command),
		MemoryLimit:            nilIfEmpty(memLimit),
		CpuLimit:               nilIfEmpty(cpuLimit),
		Replicas:               int32(replicas),
		RestartCondition:       cond,
		RestartMaxAttempts:     int32(maxAtt),
		HealthcheckCmd:         nilIfEmpty(hcCmd),
		HealthcheckInterval:    nilIfEmpty(hcInterval),
		HealthcheckTimeout:     nilIfEmpty(hcTimeout),
		HealthcheckRetries:     int32PtrIfSet(hcRetries),
		HealthcheckStartPeriod: nilIfEmpty(hcStart),
	}); err != nil {
		logFrom(r).Error("saveAdvanced: failed to update advanced settings", "err", err, "app_id", c.App.ID)
		s.flashErr(w, r, "failed to save settings")
		return
	}
	logFrom(r).Info("advanced settings updated", "app_id", c.App.ID, "app_name", c.App.Name)
	s.setFlash(w, "ok", "Advanced settings saved — applied on next deploy")
	http.Redirect(w, r, appURL(c)+"?tab=advanced", http.StatusSeeOther)
}

func nilIfEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func int32PtrIfSet(s string) *int32 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	v := int32(n)
	return &v
}

// removeAppVolume deletes a Docker named volume, retrying briefly: right after
// ServiceRemove the volume is momentarily "in use" while Swarm tears down the
// task container. A persistent failure leaves an orphaned volume (logged for
// manual cleanup) — never fatal.
func (s *Server) removeAppVolume(ctx context.Context, name string) {
	if s.engine == nil {
		return
	}
	var err error
	for i := 0; i < 12; i++ {
		if err = s.engine.VolumeRemove(ctx, name); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			slog.Error("app volume remove cancelled", "vol", name, "err", ctx.Err())
			return
		case <-time.After(time.Second):
		}
	}
	slog.Error("app volume remove gave up (still in use)", "vol", name, "err", err)
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	// Removal failure is logged but must not block the DB delete.
	if s.engine != nil {
		if err := s.engine.ServiceRemove(r.Context(), dockerName(c.App.ID)); err != nil {
			logFrom(r).Error("deleteApp: service remove failed", "err", err, "app_id", c.App.ID)
		}
	}
	if r.FormValue("destroy_data") == "on" {
		vols, err := s.q.ListVolumesByApplication(r.Context(), c.App.ID)
		if err != nil {
			logFrom(r).Error("deleteApp: list volumes failed", "err", err, "app_id", c.App.ID)
		}
		for _, v := range vols {
			s.removeAppVolume(r.Context(), docker.VolumeName(c.App.ID, v.Name))
		}
	}
	if err := s.q.DeleteApplication(r.Context(), c.App.ID); err != nil {
		logFrom(r).Error("deleteApp: failed to delete application", "err", err, "app_id", c.App.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("application deleted", "app_id", c.App.ID, "app_name", c.App.Name)
	s.setFlash(w, "ok", "Application deleted")
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

// parseEnv parses KEY=VALUE lines into a map for the deploy spec and reports any
// duplicate keys (last value wins in the map). The raw text itself is stored
// separately (env_text) so the editor preserves the user's order.
func parseEnv(raw string) (map[string]string, []string) {
	env := map[string]string{}
	var dups []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, seen := env[k]; seen {
			dups = append(dups, k)
		}
		env[k] = strings.TrimSpace(v)
	}
	return env, dups
}
