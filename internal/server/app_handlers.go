package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
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
		s.flashErrT(w, r, "flash.err.check_fields")
		return
	}
	if !validHost(domain) {
		logFrom(r).Info("createApp: invalid domain", "environment_id", e.ID, "name", name, "domain", domain)
		s.flashErrT(w, r, "flash.err.invalid_domain")
		return
	}
	if n, _ := s.q.CountDomainsByHost(r.Context(), domain); n > 0 {
		logFrom(r).Info("createApp: domain already in use", "environment_id", e.ID, "name", name, "domain", domain)
		s.flashErrT(w, r, "flash.err.domain_in_use")
		return
	}
	if sourceType == "image" && image == "" {
		logFrom(r).Info("createApp: image required for image source", "environment_id", e.ID, "name", name)
		s.flashErrT(w, r, "flash.err.specify_image")
		return
	}
	if sourceType == "dockerfile" && gitURL == "" {
		logFrom(r).Info("createApp: git URL required for dockerfile source", "environment_id", e.ID, "name", name)
		s.flashErrT(w, r, "flash.err.specify_git_url")
		return
	}
	var registryID *int64
	if v := strings.TrimSpace(r.FormValue("registry_id")); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			s.flashErrT(w, r, "flash.err.invalid_registry")
			return
		}
		reg, gerr := s.q.GetRegistry(r.Context(), n)
		if gerr != nil || reg.OrganizationID != o.ID {
			logFrom(r).Info("createApp: registry not found or org mismatch", "registry_id", n, "org_id", o.ID)
			s.flashErrT(w, r, "flash.err.invalid_registry")
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
		s.flashErrErr(w, r, "flash.err.create_app", err)
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
		s.flashErrT(w, r, "flash.err.create_domain_failed")
		return
	}
	if registryID != nil {
		if err := s.q.SetApplicationRegistry(r.Context(), db.SetApplicationRegistryParams{ID: a.ID, RegistryID: registryID}); err != nil {
			logFrom(r).Error("createApp: set registry failed", "err", err, "app_id", a.ID)
		}
	}
	logFrom(r).Info("application created", "app_id", a.ID, "environment_id", e.ID, "name", name)
	s.flashOK(w, r, "flash.ok.app_created")
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
			s.flashErrT(w, r, "flash.err.invalid_registry")
			return
		}
		reg, gerr := s.q.GetRegistry(r.Context(), n)
		if gerr != nil || reg.OrganizationID != o.ID {
			s.flashErrT(w, r, "flash.err.invalid_registry")
			return
		}
		registryID = &n
	}
	if err := s.q.SetApplicationRegistry(r.Context(), db.SetApplicationRegistryParams{ID: c.App.ID, RegistryID: registryID}); err != nil {
		logFrom(r).Error("setAppRegistry: update failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.set_registry")
		return
	}
	logFrom(r).Info("application registry set", "app_id", c.App.ID, "registry_id", registryID)
	s.flashOK(w, r, "flash.ok.registry_updated")
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

// setAppGitCredential selects (or clears) the git credential used to clone a
// private repo for source builds. The credential must belong to the app's org.
func (s *Server) setAppGitCredential(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	var gcID *int64
	if v := strings.TrimSpace(r.FormValue("git_credential_id")); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			s.flashErrT(w, r, "flash.err.invalid_git_credential")
			return
		}
		gc, gerr := s.q.GetGitCredential(r.Context(), n)
		if gerr != nil || gc.OrganizationID != c.Org.ID {
			s.flashErrT(w, r, "flash.err.invalid_git_credential")
			return
		}
		gcID = &n
	}
	if err := s.q.SetApplicationGitCredential(r.Context(), db.SetApplicationGitCredentialParams{ID: c.App.ID, GitCredentialID: gcID}); err != nil {
		logFrom(r).Error("setAppGitCredential: update failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.set_gitcred")
		return
	}
	logFrom(r).Info("application git credential set", "app_id", c.App.ID, "git_credential_id", gcID)
	s.flashOK(w, r, "flash.ok.gitcred_updated")
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

var buildArgKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validBuildText validates build_args/build_secrets the way the deploy-time
// parser (deploy.parseEnvText) reads them: blank lines and lines without '=' are
// ignored; for KEY=VALUE lines the KEY must be a valid identifier (so it cannot
// inject into a docker build flag). Keeping the two in lockstep means a save that
// validates can never produce a key the builder later rejects.
func validBuildText(raw string) error {
	for _, line := range strings.Split(raw, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		k, _, found := strings.Cut(l, "=")
		if !found {
			continue
		}
		if k = strings.TrimSpace(k); k != "" && !buildArgKeyRe.MatchString(k) {
			return fmt.Errorf("invalid build key %q: must match [A-Za-z_][A-Za-z0-9_]*", k)
		}
	}
	return nil
}

// saveBuild persists per-app build args (plaintext) and build secrets (encrypted).
func (s *Server) saveBuild(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	args := strings.TrimSpace(r.FormValue("build_args"))
	secrets := strings.TrimSpace(r.FormValue("build_secrets"))
	if err := validBuildText(args); err != nil {
		s.flashErr(w, r, err.Error())
		return
	}
	if err := validBuildText(secrets); err != nil {
		s.flashErr(w, r, err.Error())
		return
	}
	if err := s.q.UpdateApplicationBuild(r.Context(), db.UpdateApplicationBuildParams{
		ID: c.App.ID, BuildArgs: args, BuildSecrets: secret.Enc(secrets),
	}); err != nil {
		logFrom(r).Error("saveBuild: update failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.save_build")
		return
	}
	logFrom(r).Info("application build settings saved", "app_id", c.App.ID)
	s.flashOK(w, r, "flash.ok.build_saved")
	http.Redirect(w, r, appURL(c)+"?tab=advanced", http.StatusSeeOther)
}

// reconcilePlacementLabels makes the per-app krill.place.<appID> node label
// present on exactly the selected nodes (idempotent; best-effort per node).
// Returns the number of nodes whose label could not be set/cleared — a nonzero
// count means a pinned app may be unschedulable until placement is re-saved.
func (s *Server) reconcilePlacementLabels(r *http.Request, appID int64, selected []string) int {
	if s.engine == nil {
		return 0
	}
	key := fmt.Sprintf("krill.place.%d", appID)
	sel := map[string]bool{}
	for _, id := range selected {
		sel[id] = true
	}
	nodes, err := s.engine.Nodes(r.Context())
	if err != nil {
		logFrom(r).Error("reconcilePlacementLabels: list nodes failed", "err", err, "app_id", appID)
		return len(selected)
	}
	failed := 0
	for _, n := range nodes {
		if sel[n.ID] {
			if e := s.engine.NodeSetLabel(r.Context(), n.ID, key, "1"); e != nil {
				logFrom(r).Error("reconcilePlacementLabels: set label failed", "err", e, "node", n.ID, "app_id", appID)
				failed++
			}
		} else if e := s.engine.NodeDeleteLabel(r.Context(), n.ID, key); e != nil {
			logFrom(r).Error("reconcilePlacementLabels: delete label failed", "err", e, "node", n.ID, "app_id", appID)
			failed++
		}
	}
	return failed
}

// savePlacement sets an app's node placement (any | pin | global + selected
// nodes) and reconciles the per-app node labels (admin-only).
func (s *Server) savePlacement(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	mode := r.FormValue("placement_mode") // implicitly parses the form
	if mode != "any" && mode != "pin" && mode != "global" {
		s.flashErrT(w, r, "flash.err.invalid_placement")
		return
	}
	var nodes []string
	if mode != "any" {
		nodes = r.PostForm["placement_nodes"]
		// pin/global with no nodes would emit no constraint and silently behave
		// as "any" — reject so the stored mode matches the effective behavior.
		if len(nodes) == 0 {
			s.flashErrT(w, r, "flash.err.placement_no_nodes")
			return
		}
	}
	// Keep only submitted node IDs that exist in the live cluster; drop removed/
	// unknown ones (a non-live ID can't schedule and labels nothing anyway).
	droppedNodes := 0
	if s.engine != nil && len(nodes) > 0 {
		live, _ := s.engine.Nodes(r.Context())
		nodes, droppedNodes = filterLiveNodes(nodes, live)
		if mode != "any" && len(nodes) == 0 {
			s.flashErrT(w, r, "flash.err.no_live_node")
			return
		}
	}
	if err := s.q.SetApplicationPlacement(r.Context(), db.SetApplicationPlacementParams{
		ID: c.App.ID, PlacementMode: mode, PlacementNodes: strings.Join(nodes, ","),
	}); err != nil {
		logFrom(r).Error("savePlacement: update failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.internal")
		return
	}
	failed := s.reconcilePlacementLabels(r, c.App.ID, nodes)
	logFrom(r).Info("application placement saved", "app_id", c.App.ID, "mode", mode, "nodes", len(nodes), "label_failures", failed)
	if failed > 0 {
		// Persisted, but some node labels did not apply — the app may not schedule
		// onto every selected node until placement is re-saved. Surface it.
		s.flashErrT(w, r, "flash.err.placement_label_partial")
		return
	}
	if droppedNodes > 0 {
		s.flashOK(w, r, "flash.warn.dropped_dead_nodes")
	} else {
		s.flashOK(w, r, "flash.ok.placement_saved")
	}
	http.Redirect(w, r, appURL(c)+"?tab=advanced", http.StatusSeeOther)
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
	if creds, err := s.q.ListGitCredentialsByOrg(r.Context(), c.Org.ID); err != nil {
		logFrom(r).Error("appDetail: failed to list git credentials", "err", err, "org_id", c.Org.ID)
	} else {
		c.GitCredentials = creds
	}
	if s.engine != nil {
		c.Tasks, _ = s.engine.ServiceTasks(r.Context(), docker.ServiceName(c.App.ID))
		c.NodeLabels = s.nodeLabelMap(r.Context())
		if c.Role == "owner" || c.Role == "admin" {
			c.Nodes, _ = s.engine.Nodes(r.Context())
		}
	}
	// Build secrets are secrets: only decrypt for the editor when the viewer is an admin.
	if c.Role == "owner" || c.Role == "admin" {
		c.BuildSecretsPlain = secret.Dec(c.App.BuildSecrets)
	}
	c.AutoDeploy = c.App.AutoDeploy
	endpoint := "/webhooks/github/"
	if c.App.SourceType == "image" {
		endpoint = "/webhooks/deploy/"
	}
	c.WebhookURL = s.cfg.BaseURL() + endpoint + strconv.FormatInt(c.App.ID, 10)
	if c.Role == "owner" || c.Role == "admin" {
		c.WebhookSecret = secret.Dec(c.App.WebhookSecret)
	}
	if n, err := s.q.CountExposedDomainsByApplication(r.Context(), c.App.ID); err != nil {
		logFrom(r).Error("appDetail: count exposed domains", "err", err, "app_id", c.App.ID)
	} else {
		c.Exposed = n > 0
	}
	if ports, err := s.q.ListAppPorts(r.Context(), c.App.ID); err != nil {
		logFrom(r).Error("appDetail: list app ports", "err", err, "app_id", c.App.ID)
	} else {
		c.Ports = ports
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
		ldbs, _ := s.q.ListLogicalDatabasesByEnvironment(r.Context(), c.Env.ID)
		var redisInsts []db.DbInstance
		if insts, err := s.q.ListDBInstancesByOrg(r.Context(), c.Org.ID); err == nil {
			for _, in := range insts {
				if in.Engine == "redis" {
					redisInsts = append(redisInsts, in)
				}
			}
		}
		c.Ldbs = ldbs
		c.RedisInstances = redisInsts
		ldbName := map[int64]string{}
		for _, ldb := range ldbs {
			ldbName[ldb.ID] = ldb.Name + " (" + ldb.DbName + " @ " + ldb.InstanceName + ")"
		}
		redisName := map[int64]string{}
		for _, inst := range redisInsts {
			redisName[inst.ID] = inst.Name
		}
		envKeys, _ := parseEnv(c.App.EnvText)
		for _, l := range links {
			var engine, name string
			switch {
			case l.LogicalDatabaseID != nil:
				engine, name = "postgres", ldbName[*l.LogicalDatabaseID]
			case l.InstanceID != nil:
				engine, name = "redis", redisName[*l.InstanceID]
			}
			_, collides := envKeys[l.VarName]
			c.DBLinks = append(c.DBLinks, templates.DBLinkView{
				ID: l.ID, Engine: engine, DBName: name, VarName: l.VarName, Scheme: l.Scheme, Field: l.Field, Collides: collides,
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
		live, _ := s.engine.Nodes(r.Context())
		status = displayAppStatus(status, c.App.PlacementMode, c.App.PlacementNodes, live)
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
				s.flashErrT(w, r, "flash.err.update_source")
				return
			}
		}
	} else {
		image := strings.TrimSpace(r.FormValue("image"))
		tag := strings.TrimSpace(r.FormValue("tag"))
		if image != "" && tag != "" {
			if err := s.q.UpdateApplicationImage(r.Context(), db.UpdateApplicationImageParams{ID: c.App.ID, Image: image, Tag: tag}); err != nil {
				logFrom(r).Error("deployApp: failed to update application image", "err", err, "app_id", c.App.ID, "image", image)
				s.flashErrT(w, r, "flash.err.update_image")
				return
			}
		}
	}
	if s.deployer.Enqueue(c.App.ID, "manual") == 0 {
		logFrom(r).Error("deployApp: deployment not accepted (queue full or shutting down)", "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.queue_deploy")
		return
	}
	logFrom(r).Info("deploy enqueued", "app_id", c.App.ID, "app_name", c.App.Name)
	s.flashOK(w, r, "flash.ok.deploy_queued")
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
		s.flashErrT(w, r, "flash.err.queue_rebuild")
		return
	}
	logFrom(r).Info("rebuild enqueued", "app_id", c.App.ID, "app_name", c.App.Name)
	s.flashOK(w, r, "flash.ok.rebuild_queued")
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
			s.flashErrT(w, r, "flash.err.reload_app")
			return
		}
	}
	logFrom(r).Info("application reloaded", "app_id", c.App.ID, "app_name", c.App.Name)
	s.flashOK(w, r, "flash.ok.reload_requested")
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
			s.flashErrT(w, r, "flash.err.stop_application")
			return
		}
	}
	if err := s.q.UpdateApplicationStatus(r.Context(), db.UpdateApplicationStatusParams{ID: c.App.ID, Status: deploy.StatusIdle}); err != nil {
		logFrom(r).Error("stopApp: failed to update status", "err", err, "app_id", c.App.ID)
	}
	logFrom(r).Info("application stopped", "app_id", c.App.ID, "app_name", c.App.Name)
	s.flashOK(w, r, "flash.ok.app_stopped")
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

func (s *Server) saveEnv(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	raw := r.FormValue("env")
	if _, dups := parseEnv(raw); len(dups) > 0 {
		s.flashErr(w, r, i18n.Tf(r.Context(), "flash.err.duplicate_var", dups[0]))
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
	s.flashOK(w, r, "flash.ok.env_saved")
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
		s.flashErrT(w, r, "flash.err.invalid_mem")
		return
	}
	cpuLimit := strings.TrimSpace(r.FormValue("cpu_limit"))
	if _, err := docker.ParseNanoCPUs(cpuLimit); err != nil {
		s.flashErrT(w, r, "flash.err.invalid_cpu")
		return
	}
	replicas, err := strconv.Atoi(strings.TrimSpace(r.FormValue("replicas")))
	if err != nil || replicas < 1 || replicas > 20 {
		s.flashErrT(w, r, "flash.err.replicas_range")
		return
	}
	cond := r.FormValue("restart_condition")
	if cond != "any" && cond != "on-failure" && cond != "none" {
		s.flashErrT(w, r, "flash.err.invalid_restart_cond")
		return
	}
	maxAttStr := strings.TrimSpace(r.FormValue("restart_max_attempts"))
	if maxAttStr == "" {
		maxAttStr = "0"
	}
	maxAtt, err := strconv.Atoi(maxAttStr)
	if err != nil || maxAtt < 0 {
		s.flashErrT(w, r, "flash.err.max_attempts")
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
				s.flashErrT(w, r, "flash.err.invalid_hc_duration")
				return
			}
		}
		if hcRetries != "" {
			if n, perr := strconv.Atoi(hcRetries); perr != nil || n < 0 {
				s.flashErrT(w, r, "flash.err.hc_retries")
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
		s.flashErrT(w, r, "flash.err.save_settings")
		return
	}
	logFrom(r).Info("advanced settings updated", "app_id", c.App.ID, "app_name", c.App.Name)
	s.flashOK(w, r, "flash.ok.advanced_saved")
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
		// Drop the per-app placement labels so removed apps don't orphan
		// krill.place.<appID> labels on cluster nodes.
		s.reconcilePlacementLabels(r, c.App.ID, nil)
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
	s.flashOK(w, r, "flash.ok.app_deleted")
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
