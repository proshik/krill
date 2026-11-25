package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/envtext"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/topology"
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
	if reserved, err := s.hostReservedByPanel(r.Context(), domain); err != nil || reserved {
		logFrom(r).Info("createApp: domain reserved by the panel", "environment_id", e.ID, "domain", domain, "err", err)
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
	if s.hosts == nil {
		logFrom(r).Error("createApp: host-sharing store unavailable")
		s.flashErrT(w, r, "flash.err.create_app")
		return
	}
	a, err := s.hosts.CreateApplication(r.Context(), db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: name, Image: image, Tag: tag, Domain: domain, Port: int32(port),
		EnvText: r.FormValue("env"), SourceType: sourceType,
		GitUrl: gitURL, GitBranch: gitBranch, DockerfilePath: dockerfilePath,
	})
	if err != nil {
		var conflict *hostshare.ConflictError
		if errors.As(err, &conflict) {
			s.flashErrT(w, r, "flash.err.domain_in_use")
			return
		}
		logFrom(r).Error("createApp: failed to create application", "err", err, "environment_id", e.ID, "name", name)
		s.flashErrErr(w, r, "flash.err.create_app", err)
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
	args := strings.TrimSpace(r.FormValue("build_args")) // also populates r.Form
	if err := validBuildText(args); err != nil {
		s.flashErr(w, r, err.Error())
		return
	}
	// Absent field != "clear the secrets". The Advanced tab omits the editor
	// when the stored value cannot be decrypted, and clearing on submit would
	// destroy secrets that restoring the original key would bring back.
	storedSecrets := c.App.BuildSecrets
	if r.Form.Has("build_secrets") {
		secrets := strings.TrimSpace(r.FormValue("build_secrets"))
		if err := validBuildText(secrets); err != nil {
			s.flashErr(w, r, err.Error())
			return
		}
		storedSecrets = secret.Enc(secrets)
	}
	if err := s.q.UpdateApplicationBuild(r.Context(), db.UpdateApplicationBuildParams{
		ID: c.App.ID, BuildArgs: args, BuildSecrets: storedSecrets,
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
// Callers hold AppPlacement through persistence and these label mutations.
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
	key := oplock.AppPlacement(c.App.ID)
	if !oplock.TryAcquire(key) {
		s.flashErrT(w, r, "flash.err.placement_busy")
		return
	}
	defer oplock.Release(key)

	mode := r.FormValue("placement_mode") // implicitly parses the form
	if mode != "any" && mode != "pin" && mode != "global" {
		s.flashErrT(w, r, "flash.err.invalid_placement")
		return
	}
	// Switching to "any" on a multi-node cluster would leave owned volumes chowned
	// only on the control-plane, so a replica on a worker gets an unwritable volume.
	if mode == "any" && s.clusterHasWorkers(r) {
		vols, _ := s.q.ListVolumesByApplication(r.Context(), c.App.ID)
		for _, v := range vols {
			if v.Owner != nil {
				s.flashErrT(w, r, "flash.err.placement_any_owned_volume")
				return
			}
		}
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
		live, nerr := s.engine.Nodes(r.Context())
		if nerr != nil {
			// An unreadable list would drop every submitted node and report
			// "no live node" — a misleading verdict on healthy nodes. Refuse
			// instead of guessing.
			logFrom(r).Error("savePlacement: node list unavailable, refusing to validate placement", "err", nerr, "app_id", c.App.ID)
			s.flashErrT(w, r, "flash.err.internal")
			return
		}
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

// appTab maps a requested tab onto the sheet's tabs. The names of the
// previous layout (general, env, advanced, volumes) still lead somewhere: they
// are in redirects and links people kept.
func appTab(tab string, admin bool) string {
	switch tab {
	case "env":
		tab = "variables"
	case "advanced", "volumes":
		tab = "settings"
	case "general", "":
		tab = "deployments"
	}
	switch tab {
	case "deployments", "logs", "domains", "metrics", "settings":
		return tab
	case "variables", "terminal":
		// Env values are secrets and the terminal runs commands: members, who
		// are read-only viewers, get the deployments tab instead.
		if admin {
			return tab
		}
	}
	return "deployments"
}

func (s *Server) appDetail(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	tab := appTab(r.URL.Query().Get("tab"), isAdminRole(c.Role))
	if !s.fillAppSheet(w, r, &c, tab) {
		return
	}
	render(w, r, http.StatusOK, templates.AppDetail(c, tab))
}

// fillAppSheet loads what the app's sheet shows on the given tab, plus the
// environment canvas under it. It answers the request itself on failure.
func (s *Server) fillAppSheet(w http.ResponseWriter, r *http.Request, c *templates.AppCtx, tab string) bool {
	ctx := r.Context()
	admin := isAdminRole(c.Role)
	envs, err := s.q.ListEnvironments(ctx, c.Project.ID)
	if err != nil {
		logFrom(r).Error("appDetail: list environments", "err", err, "project_id", c.Project.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	c.Shell = s.envShell(ctx, c.Org, c.Role, c.Project, c.Env, envs, c.App.Name+", "+c.Project.Name+" "+c.Env.Name, "canvas", "is-canvas has-sheet")
	d := s.loadEnvData(ctx, c.Org.ID, c.Project.ID, c.Env)
	c.Canvas = s.canvasFor(ctx, c.Org, c.Role, c.Project, c.Env, d, topology.AppKey(c.App.ID), false)
	for _, a := range d.apps {
		if a.ID == c.App.ID {
			c.App.Status = a.Status // live, as on the canvas
		}
	}
	if p, checked := c.Canvas.PendingApps[c.App.ID]; checked {
		c.Pending = p
	} else {
		c.Pending = s.pendingChanges(ctx, []db.Application{c.App})[c.App.ID]
	}
	if doms, err := s.q.ListDomainsByApplication(ctx, c.App.ID); err != nil {
		logFrom(r).Error("appDetail: failed to list domains", "err", err, "app_id", c.App.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	} else {
		c.Domains = doms
	}
	if n, err := s.q.CountExposedDomainsByApplication(ctx, c.App.ID); err != nil {
		logFrom(r).Error("appDetail: count exposed domains", "err", err, "app_id", c.App.ID)
	} else {
		c.Exposed = n > 0
	}
	if s.engine != nil {
		c.NodeLabels = s.nodeLabelMap(ctx)
		c.NodeNames = map[string]string{}
		if nodes, err := s.engine.Nodes(ctx); err == nil {
			for _, n := range nodes {
				c.NodeNames[n.ID] = nodeDisplayName(n, c.NodeLabels)
			}
		}
	}

	c.Running = -1
	switch tab {
	case "terminal":
		// The terminal needs a running replica, not a status: a first deploy
		// reads "deploying" before any container exists.
		if s.engine != nil {
			if st, err := s.engine.ServiceState(ctx, docker.ServiceName(c.App.ID)); err == nil {
				c.Running = int(st.Running)
			} else {
				logFrom(r).Warn("appDetail: service state", "err", err, "app_id", c.App.ID)
			}
		}
	case "deployments":
		if s.engine != nil {
			tasks, err := s.engine.ServiceTasks(ctx, docker.ServiceName(c.App.ID))
			if err != nil {
				logFrom(r).Warn("appDetail: service tasks", "err", err, "app_id", c.App.ID)
			}
			c.Tasks = tasks
		}
		deps, err := s.q.ListDeploymentSummariesByApplication(ctx, c.App.ID)
		if err != nil {
			logFrom(r).Error("appDetail: failed to list deployments", "err", err, "app_id", c.App.ID)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return false
		}
		c.Deps = deps
	case "metrics":
		c.MetricsEnabled, c.MetricsTokenEnv = c.App.MetricsEnabled, c.App.MetricsTokenEnv
		eps, err := s.q.ListMetricsEndpointsByApplication(ctx, c.App.ID)
		if err != nil {
			logFrom(r).Error("appDetail: read metrics endpoints failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return false
		}
		c.MetricsEndpoints = eps
		if admin && c.App.MetricsToken != nil {
			token, err := secret.Dec(*c.App.MetricsToken)
			if err != nil {
				logFrom(r).Warn("appDetail: metrics token unreadable", "app_id", c.App.ID)
				c.MetricsTokenUnreadable = true
			} else {
				c.MetricsToken = token
			}
		}
	case "domains":
		shares, err := s.q.ListHostSharesByApplication(ctx, c.App.ID)
		if err != nil {
			logFrom(r).Error("appDetail: failed to list host shares", "err", err, "app_id", c.App.ID)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return false
		}
		c.HostShares = map[int64][]db.ListHostSharesByApplicationRow{}
		for _, sh := range shares {
			c.HostShares[sh.DomainID] = append(c.HostShares[sh.DomainID], sh)
		}
	case "variables":
		s.fillAppLinks(r, c, d)
	case "settings":
		if !s.fillAppSettings(w, r, c) {
			return false
		}
	}
	return true
}

// fillAppLinks loads the variables tab: the app's database and bucket links
// and what it could link to.
func (s *Server) fillAppLinks(r *http.Request, c *templates.AppCtx, d envData) {
	ctx := r.Context()
	links, lerr := s.q.ListDBLinksByApplication(ctx, c.App.ID)
	if lerr != nil {
		logFrom(r).Error("appDetail: list db links", "err", lerr, "app_id", c.App.ID)
	}
	c.Ldbs = d.ldbs
	var linkInsts []db.DbInstance
	for _, in := range d.insts {
		if in.Engine != "postgres" {
			linkInsts = append(linkInsts, in)
		}
	}
	sort.Slice(linkInsts, func(i, j int) bool { return linkInsts[i].Name < linkInsts[j].Name })
	c.LinkInstances = linkInsts
	ldbName := map[int64]string{}
	for _, ldb := range d.ldbs {
		ldbName[ldb.ID] = ldb.Name + " (" + ldb.DbName + ", " + ldb.InstanceName + ")"
	}
	envKeys, _ := parseEnv(c.App.EnvText)
	for _, l := range links {
		var engine, name string
		switch {
		case l.LogicalDatabaseID != nil:
			engine, name = "postgres", ldbName[*l.LogicalDatabaseID]
		case l.InstanceID != nil:
			in := d.insts[*l.InstanceID]
			engine, name = in.Engine, in.Name
		}
		_, collides := envKeys[l.VarName]
		c.DBLinks = append(c.DBLinks, templates.DBLinkView{
			ID: l.ID, Engine: engine, DBName: name, VarName: l.VarName, Scheme: l.Scheme, Field: l.Field, Collides: collides,
		})
	}
	c.EnvBuckets = d.buckets
	bucketName := map[int64]string{}
	for _, b := range d.buckets {
		bucketName[b.ID] = b.Name
	}
	bLinks, blerr := s.q.ListBucketLinksByApplication(ctx, c.App.ID)
	if blerr != nil {
		logFrom(r).Error("appDetail: list bucket links", "err", blerr, "app_id", c.App.ID)
	}
	for _, l := range bLinks {
		_, collides := envKeys[l.VarName]
		c.BucketLinks = append(c.BucketLinks, templates.BucketLinkView{ID: l.ID, Bucket: bucketName[l.BucketID], VarName: l.VarName, Field: l.Field, Collides: collides})
	}
}

// fillAppSettings loads the settings tab: source, credentials, build
// secrets, auto-deploy, runtime, placement, ports and volumes with their
// backups. Secrets are decrypted for admins only.
func (s *Server) fillAppSettings(w http.ResponseWriter, r *http.Request, c *templates.AppCtx) bool {
	ctx := r.Context()
	admin := isAdminRole(c.Role)
	if regs, err := s.q.ListRegistriesByOrg(ctx, c.Org.ID); err != nil {
		logFrom(r).Error("appDetail: failed to list registries", "err", err, "org_id", c.Org.ID)
	} else {
		c.Registries = regs
	}
	if creds, err := s.q.ListGitCredentialsByOrg(ctx, c.Org.ID); err != nil {
		logFrom(r).Error("appDetail: failed to list git credentials", "err", err, "org_id", c.Org.ID)
	} else {
		c.GitCredentials = creds
	}
	if s.engine != nil && admin {
		nodes, err := s.engine.Nodes(ctx)
		if err != nil {
			logFrom(r).Warn("appDetail: list nodes", "err", err)
		}
		c.Nodes = nodes
	}
	if admin {
		// Never render an empty editor for secrets we simply cannot read: the
		// admin would save the blank field and wipe the stored values.
		if plain, derr := secret.Dec(c.App.BuildSecrets); derr != nil {
			logFrom(r).Error("appDetail: build secrets undecryptable", "err", derr, "app_id", c.App.ID)
			c.BuildSecretsUnreadable = true
		} else {
			c.BuildSecretsPlain = plain
		}
		if ws, derr := secret.Dec(c.App.WebhookSecret); derr != nil {
			logFrom(r).Error("appDetail: webhook secret undecryptable", "err", derr, "app_id", c.App.ID)
		} else {
			c.WebhookSecret = ws
		}
	}
	c.AutoDeploy = c.App.AutoDeploy
	endpoint := "/webhooks/github/"
	if c.App.SourceType == "image" {
		endpoint = "/webhooks/deploy/"
	}
	c.WebhookURL = s.shownBaseURL(r) + endpoint + strconv.FormatInt(c.App.ID, 10)
	if ports, err := s.q.ListAppPorts(ctx, c.App.ID); err != nil {
		logFrom(r).Error("appDetail: list app ports", "err", err, "app_id", c.App.ID)
	} else {
		c.Ports = ports
	}
	c.Runtime = s.runtimeView(r, c.App)
	if !admin {
		return true
	}
	vols, err := s.q.ListVolumesByApplication(ctx, c.App.ID)
	if err != nil {
		logFrom(r).Error("appDetail: failed to list volumes", "err", err, "app_id", c.App.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	c.Volumes = vols
	if dests, derr := s.q.ListDestinationsByOrg(ctx, c.Org.ID); derr != nil {
		logFrom(r).Error("appDetail: failed to list destinations", "err", derr, "org_id", c.Org.ID)
	} else {
		c.Destinations = dests
	}
	base := appURL(*c)
	c.VolumeBlocks = map[int64]templates.BackupBlock{}
	for _, v := range vols {
		b := templates.BackupBlock{
			ID: "vbk-" + strconv.FormatInt(v.ID, 10), Title: i18n.Tf(ctx, "volumes.backups_of", v.Name),
			Hint: i18n.T(ctx, "volumes.backup.hot_caveat"), Destinations: c.Destinations,
			AddURL: base + "/volumes/" + strconv.FormatInt(v.ID, 10) + "/backups", OrgID: c.Org.ID,
			ReturnURL: base + "?tab=settings#s-vol", Admin: true,
		}
		bks, berr := s.q.ListVolumeBackupsByVolume(ctx, v.ID)
		if berr != nil {
			logFrom(r).Error("appDetail: failed to list volume backups", "err", berr, "volume_id", v.ID)
		}
		for _, bk := range bks {
			id := strconv.FormatInt(bk.ID, 10)
			b.Configs = append(b.Configs, templates.BackupConfig{
				ID: bk.ID, Storage: destNameOf(c.Destinations, bk.DestinationID), Schedule: bk.Schedule, Retention: bk.Retention,
				Enabled: bk.Enabled, LastRun: bk.LastRunAt.Time, Ran: bk.LastRunAt.Valid, LastStatus: bk.LastStatus, LastError: bk.LastError,
				RunURL: base + "/volumes/backups/" + id + "/run", ToggleURL: base + "/volumes/backups/" + id + "/toggle",
				DeleteURL: base + "/volumes/backups/" + id + "/delete", ObjectsURL: base + "/volumes/backups/" + id + "/objects",
			})
		}
		c.VolumeBlocks[v.ID] = b
	}
	return true
}

// destNameOf is a backup storage's name for display.
func destNameOf(dests []db.Destination, id int64) string {
	for _, d := range dests {
		if d.ID == id {
			return d.Name
		}
	}
	return "#" + strconv.FormatInt(id, 10)
}

func (s *Server) appStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	status := c.App.Status
	running := -1
	if s.engine != nil {
		if st, err := s.engine.ServiceState(r.Context(), dockerName(c.App.ID)); err == nil {
			running = int(st.Running)
			status = deploy.DisplayStatus(st, c.App.Status, s.deployedApps(r.Context(), []int64{c.App.ID})[c.App.ID])
		} else {
			logFrom(r).Error("appStatus: failed to query engine service state", "err", err, "app_id", c.App.ID)
		}
		live, nerr := s.engine.Nodes(r.Context())
		status = displayAppStatus(status, c.App.PlacementMode, c.App.PlacementNodes, live, nerr == nil)
	}
	render(w, r, http.StatusOK, templates.AppStateBadge(status, appURL(c)+"/status", running))
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
	regPW, derr := secret.Dec(reg.Password)
	if derr != nil {
		logFrom(r).Error("appTags: registry password undecryptable", "err", derr, "registry_id", reg.ID)
		http.Error(w, "registry credentials unreadable", http.StatusBadRequest)
		return
	}
	tags, err := docker.RegistryListTags(r.Context(), reg.RegistryUrl, reg.Username, regPW, repo, s.cfg.AllowPrivateEgress)
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
		if gitBranch == "" {
			gitBranch = c.App.GitBranch // don't clear a configured branch
		}
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
	}
	// An image app's new reference travels with the deploy job and reaches the
	// application row only once the job is accepted: written here first, a
	// refused deploy would leave a tag nobody deployed, and a deploy already in
	// flight could pick it up.
	var err error
	image := strings.TrimSpace(r.FormValue("image"))
	tag := strings.TrimSpace(r.FormValue("tag"))
	if c.App.SourceType != "dockerfile" && image != "" && tag != "" {
		_, err = s.deployer.EnqueueImageErr(c.App.ID, deploy.TriggerManual, image, tag)
	} else {
		_, err = s.deployer.EnqueueErr(c.App.ID, deploy.TriggerManual)
	}
	if err != nil {
		logFrom(r).Warn("deployApp: deployment not accepted", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, deployRefusalKey(err, "flash.err.queue_deploy"))
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
	if _, err := s.deployer.EnqueueRebuildErr(c.App.ID, deploy.TriggerManual); err != nil {
		logFrom(r).Warn("rebuildApp: rebuild not accepted", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, deployRefusalKey(err, "flash.err.queue_rebuild"))
		return
	}
	logFrom(r).Info("rebuild enqueued", "app_id", c.App.ID, "app_name", c.App.Name)
	s.flashOK(w, r, "flash.ok.rebuild_queued")
	http.Redirect(w, r, appURL(c)+"?tab=deployments", http.StatusSeeOther)
}

// reloadApp force-restarts the running containers without rebuilding/re-pulling.
// A stopped (or never deployed) app is deployed instead: a restart would bring
// it back on the spec it was stopped with — after the organization network
// migration, that is the shared network, away from its databases.
func (s *Server) reloadApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if s.engine != nil {
		st, err := s.engine.ServiceState(r.Context(), dockerName(c.App.ID))
		if err != nil {
			logFrom(r).Error("reloadApp: failed to read service state", "err", err, "app_id", c.App.ID)
			s.flashErrT(w, r, "flash.err.reload_app")
			return
		}
		if !st.Found || st.Desired == 0 {
			if s.deployer == nil {
				s.flashErrT(w, r, "flash.err.queue_deploy")
				return
			}
			if _, err := s.deployer.EnqueueErr(c.App.ID, deploy.TriggerManual); err != nil {
				logFrom(r).Warn("reloadApp: deployment of a stopped app not accepted", "err", err, "app_id", c.App.ID)
				s.flashErrT(w, r, deployRefusalKey(err, "flash.err.queue_deploy"))
				return
			}
			logFrom(r).Info("reload of a stopped application enqueued as a deploy", "app_id", c.App.ID, "app_name", c.App.Name)
			s.flashOK(w, r, "flash.ok.deploy_queued")
			http.Redirect(w, r, appURL(c)+"?tab=deployments", http.StatusSeeOther)
			return
		}
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

// stopApp scales the app's service to zero replicas and marks it idle. A
// deploy of the app that has not applied its service spec yet is cancelled
// first (deploy.Deployer.StopApp), so it cannot bring the app back up.
func (s *Server) stopApp(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	scale := func(ctx context.Context) error {
		if s.engine == nil {
			return nil
		}
		return s.engine.ServiceScale(ctx, dockerName(c.App.ID), 0)
	}
	var cancelled []int64
	var err error
	if s.deployer != nil {
		cancelled, err = s.deployer.StopApp(r.Context(), c.App.ID, s.actorName(r.Context(), auth.UserID(r.Context())), scale)
	} else {
		err = scale(r.Context())
	}
	if err != nil {
		logFrom(r).Error("stopApp: failed to scale to zero", "err", err, "app_id", c.App.ID, "cancelled_deploys", cancelled)
		s.flashErrT(w, r, "flash.err.stop_application")
		return
	}
	if err := s.q.UpdateApplicationStatus(r.Context(), db.UpdateApplicationStatusParams{ID: c.App.ID, Status: deploy.StatusIdle}); err != nil {
		logFrom(r).Error("stopApp: failed to update status", "err", err, "app_id", c.App.ID)
	}
	logFrom(r).Info("application stopped", "app_id", c.App.ID, "app_name", c.App.Name, "cancelled_deploys", cancelled)
	if len(cancelled) > 0 {
		s.flashOK(w, r, "flash.ok.stop_cancelled_deploy")
	} else {
		s.flashOK(w, r, "flash.ok.app_stopped")
	}
	http.Redirect(w, r, appURL(c), http.StatusSeeOther)
}

// actorName names a user in deploy logs and deployment errors (their email);
// the user id when the account cannot be read.
func (s *Server) actorName(ctx context.Context, uid int64) string {
	if uid == 0 {
		return ""
	}
	u, err := s.q.GetUserByID(ctx, uid)
	if err != nil || u.Email == "" {
		return fmt.Sprintf("user %d", uid)
	}
	return u.Email
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
	if c.App.MetricsEnabled && deploy.EnvNameInUse(raw, nil, c.App.MetricsTokenEnv) {
		s.flashErr(w, r, i18n.Tf(r.Context(), "flash.err.metrics_env_taken", c.App.MetricsTokenEnv))
		return
	}
	publicKeys := envtext.PublicKeys(c.App.EnvText, c.App.EnvPublicKeys)
	if _, present := r.Form["env_public_keys"]; present {
		publicKeys = strings.Split(r.FormValue("env_public_keys"), "\n")
	}
	if err := s.q.UpdateApplicationEnvClassified(r.Context(), db.UpdateApplicationEnvClassifiedParams{
		ID: c.App.ID, EnvText: raw, EnvPublicKeys: envtext.PublicKeys(raw, publicKeys),
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
		PredeployCommand:       strings.TrimSpace(r.FormValue("predeploy_command")),
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
	key := oplock.AppPlacement(c.App.ID)
	if !oplock.TryAcquire(key) {
		s.flashErrT(w, r, "flash.err.placement_busy")
		return
	}
	defer oplock.Release(key)

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
	s.reloadVolumeBackupSchedules() // app volumes (and their backups) cascaded away
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
// parseEnv derives the environment from raw env_text and reports any key that
// appears more than once, so the save handler can refuse a file whose meaning
// depends on which occurrence a reader happens to keep.
func parseEnv(raw string) (map[string]string, []string) {
	return envtext.Map(raw)
}

// deployRefusalKey is the flash key saying why a deploy was not accepted;
// fallback covers a store failure.
func deployRefusalKey(err error, fallback string) string {
	switch {
	case errors.Is(err, deploy.ErrDeployInFlight):
		return "flash.err.deploy_in_flight"
	case errors.Is(err, deploy.ErrOrgDeployLimit):
		return "flash.err.deploy_org_limit"
	case errors.Is(err, deploy.ErrQueueFull):
		return "flash.err.deploy_queue_full"
	case errors.Is(err, deploy.ErrDeployerStopped):
		return "flash.err.deploy_shutting_down"
	}
	return fallback
}

// runtimeTaskLimit bounds the task history the Advanced tab shows.
const runtimeTaskLimit = 8

// runtimeView reports the limits a deploy of app applies — its own, or the
// instance default it falls back to — and its recent tasks.
func (s *Server) runtimeView(r *http.Request, app db.Application) templates.RuntimeView {
	v := templates.RuntimeView{MemSource: "none", CPUSource: "none",
		MemDefault: strings.TrimSpace(s.cfg.DefaultMemoryLimit), CPUDefault: strings.TrimSpace(s.cfg.DefaultCPULimit)}
	switch {
	case app.MemoryLimit != nil && strings.TrimSpace(*app.MemoryLimit) != "":
		v.MemLimit, v.MemSource = *app.MemoryLimit, "explicit"
	case strings.TrimSpace(s.cfg.DefaultMemoryLimit) != "":
		v.MemLimit, v.MemSource = s.cfg.DefaultMemoryLimit, "default"
	}
	switch {
	case app.CpuLimit != nil && strings.TrimSpace(*app.CpuLimit) != "":
		v.CPULimit, v.CPUSource = *app.CpuLimit, "explicit"
	case strings.TrimSpace(s.cfg.DefaultCPULimit) != "":
		v.CPULimit, v.CPUSource = s.cfg.DefaultCPULimit, "default"
	}
	th, ok := s.engine.(docker.TaskHistory)
	if !ok {
		v.TasksUnavailable = true
		return v
	}
	tasks, err := th.ServiceTaskHistory(r.Context(), dockerName(app.ID), runtimeTaskLimit)
	if err != nil {
		logFrom(r).Warn("runtimeView: task history unavailable", "err", err, "app_id", app.ID)
		v.TasksUnavailable = true
		return v
	}
	v.Tasks = tasks
	return v
}
