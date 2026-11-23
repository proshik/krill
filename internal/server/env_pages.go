package server

import (
	"time"

	"context"
	"github.com/jackc/pgx/v5/pgtype"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/envclone"
	"github.com/proshik/krill/internal/topology"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// envHealth returns each environment's health for the switcher: err when an
// app's deploy failed, warn while one deploys, ok when every app runs, idle
// when there is nothing running at all. One query and one Swarm call for all.
func (s *Server) envHealth(ctx context.Context, envs []db.Environment) map[int64]string {
	out := make(map[int64]string, len(envs))
	ids := make([]int64, 0, len(envs))
	for _, e := range envs {
		ids = append(ids, e.ID)
		out[e.ID] = "idle"
	}
	if len(ids) == 0 {
		return out
	}
	apps, err := s.q.ListApplicationsByEnvironmentIDs(ctx, ids)
	if err != nil {
		slog.Warn("envHealth: list applications", "err", err)
		return out
	}
	s.deriveCardStatuses(ctx, apps, nil)
	// The same order the projects page summarises an environment in: one
	// failed app makes it red, one deploying yellow, and it is green only
	// when every app runs — one stopped app keeps it grey.
	rank := map[string]int{"ok": 0, "idle": 1, "warn": 2, "err": 3}
	seen := map[int64]bool{}
	for _, a := range apps {
		k := "idle"
		switch a.Status {
		case deploy.StatusRunning:
			k = "ok"
		case deploy.StatusDeploying:
			k = "warn"
		case deploy.StatusError:
			k = "err"
		}
		if !seen[a.EnvironmentID] || rank[k] > rank[out[a.EnvironmentID]] {
			out[a.EnvironmentID] = k
		}
		seen[a.EnvironmentID] = true
	}
	return out
}

// envShell is the frame of an environment page: the environment bar with the
// switcher's health.
func (s *Server) envShell(ctx context.Context, o db.Organization, role string, p db.Project, active db.Environment, envs []db.Environment, title, tab, body string) templates.Shell {
	health := s.envHealth(ctx, envs)
	picks := make([]templates.EnvPick, 0, len(envs))
	for _, e := range envs {
		picks = append(picks, templates.EnvPick{ID: e.ID, Name: e.Name, Health: health[e.ID]})
	}
	return templates.Shell{
		Title: title, Org: o, Role: role, Tab: tab, Body: body,
		Env: &templates.EnvBar{Project: p, Env: active, Envs: picks},
	}
}

// envChain loads org, project, the project's environments and the one in the
// path; it answers 404 itself when any link of the chain is foreign.
func (s *Server) envChain(w http.ResponseWriter, r *http.Request) (db.Organization, string, db.Project, db.Environment, []db.Environment, bool) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return db.Organization{}, "", db.Project{}, db.Environment{}, nil, false
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return db.Organization{}, "", db.Project{}, db.Environment{}, nil, false
	}
	e, ok := s.loadEnvironment(w, r, p.ID)
	if !ok {
		return db.Organization{}, "", db.Project{}, db.Environment{}, nil, false
	}
	envs, err := s.q.ListEnvironments(r.Context(), p.ID)
	if err != nil {
		logFrom(r).Error("envChain: list environments", "err", err, "project_id", p.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return db.Organization{}, "", db.Project{}, db.Environment{}, nil, false
	}
	return o, role, p, e, envs, true
}

// envData is what an environment's pages are built from.
type envData struct {
	apps      []db.Application
	ldbs      []db.ListLogicalDatabasesByEnvironmentRow
	buckets   []db.ListS3BucketsByEnvironmentRow
	insts     map[int64]db.DbInstance // the organization's servers, live status
	view      topology.EnvView
	resources map[string]topology.EnvResource
}

func (s *Server) loadEnvData(ctx context.Context, orgID, projID int64, env db.Environment) envData {
	var d envData
	var err error
	if d.apps, err = s.q.ListApplicationsByEnvironment(ctx, env.ID); err != nil {
		slog.Warn("loadEnvData: list applications", "err", err, "environment_id", env.ID)
	}
	if d.ldbs, err = s.q.ListLogicalDatabasesByEnvironment(ctx, env.ID); err != nil {
		slog.Warn("loadEnvData: list databases", "err", err, "environment_id", env.ID)
	}
	if d.buckets, err = s.q.ListS3BucketsByEnvironment(ctx, env.ID); err != nil {
		slog.Warn("loadEnvData: list buckets", "err", err, "environment_id", env.ID)
	}
	sort.SliceStable(d.apps, func(i, j int) bool { return d.apps[i].ID < d.apps[j].ID })
	s.deriveCardStatuses(ctx, d.apps, d.ldbs)
	d.insts = map[int64]db.DbInstance{}
	if all, err := s.q.ListDBInstancesByOrg(ctx, orgID); err == nil {
		names := make([]string, 0, len(all))
		for _, in := range all {
			names = append(names, in.AppName)
		}
		var states map[string]docker.ServiceState
		if s.engine != nil && len(names) > 0 {
			if states, err = s.engine.ServiceStates(ctx, names); err != nil {
				slog.Warn("loadEnvData: server states", "err", err)
			}
		}
		for _, in := range all {
			if st, ok := states[in.AppName]; ok {
				in.Status = deploy.DeriveStatus(st, in.Status)
			}
			d.insts[in.ID] = in
		}
	} else {
		slog.Warn("loadEnvData: list servers", "err", err)
	}
	for i := range d.buckets {
		if in, ok := d.insts[d.buckets[i].InstanceID]; ok {
			d.buckets[i].InstanceStatus = in.Status
		}
	}
	d.view, d.resources = s.envViewResources(ctx, orgID, projID, env, d.apps, d.ldbs, d.buckets)
	return d
}

// placement returns each Swarm service's running node names and the cluster
// node names in display order (control plane first).
//
// running maps a service to the display names of the nodes its tasks run on;
// runningIDs to their Swarm node IDs, in the same order. Display names can
// repeat (two workers may share a label), so anything that matches a card to
// a node uses the IDs.
func (s *Server) placement(ctx context.Context) (running, runningIDs map[string][]string, nodes []docker.SwarmNode, labels map[string]string) {
	running, runningIDs = map[string][]string{}, map[string][]string{}
	if s.engine == nil {
		return running, runningIDs, nil, nil
	}
	nodes, err := s.engine.Nodes(ctx)
	if err != nil {
		slog.Warn("placement: list nodes", "err", err)
	}
	labels = s.nodeLabelMap(ctx)
	name := map[string]string{}
	for _, n := range nodes {
		name[n.ID] = nodeDisplayName(n, labels)
	}
	tasks, err := s.engine.Tasks(ctx)
	if err != nil {
		slog.Warn("placement: list tasks", "err", err)
	}
	ref := make([]topology.Task, 0, len(tasks))
	for _, t := range tasks {
		ref = append(ref, topology.Task{Service: t.ServiceName, Node: t.NodeID, Running: t.State == "running"})
	}
	for svc, ids := range topology.ResolveNodes(ref) {
		var known []string
		for _, id := range ids {
			if name[id] != "" {
				known = append(known, id)
			}
		}
		sort.SliceStable(known, func(i, j int) bool { return name[known[i]] < name[known[j]] })
		for _, id := range known {
			running[svc] = append(running[svc], name[id])
		}
		runningIDs[svc] = known
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		li, lj := nodes[i].Leader || nodes[i].Role == "manager", nodes[j].Leader || nodes[j].Role == "manager"
		if li != lj {
			return li
		}
		return name[nodes[i].ID] < name[nodes[j].ID]
	})
	return running, runningIDs, nodes, labels
}

// canvasFor builds the environment canvas. current is the key of the card
// whose sheet is open.
func (s *Server) canvasFor(ctx context.Context, o db.Organization, role string, p db.Project, env db.Environment, d envData, current string, ghost bool) templates.CanvasView {
	base := envURL(o.ID, p.ID, env.ID)
	running, runningIDs, nodes, labels := s.placement(ctx)
	v := templates.CanvasView{
		Title:     p.Name + ", " + env.Name,
		Cards:     map[string]templates.CanvasCard{},
		StatusURL: base + "/statuses",
		Current:   current,
		Ghost:     ghost,
	}
	if isAdminRole(role) {
		v.NewAppURL = base + "?new=app"
	}
	all := make([]topology.EnvResource, 0, len(d.resources))
	for _, r := range d.resources {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })
	v.Plane = topology.LayoutCanvas(d.view, all)
	v.Empty = len(d.apps) == 0 && len(all) == 0

	// Apps.
	appIDs := make([]int64, 0, len(d.apps))
	for _, a := range d.apps {
		appIDs = append(appIDs, a.ID)
	}
	primary := map[int64]db.Domain{}
	if len(appIDs) > 0 {
		if doms, err := s.q.ListDomainsByApplicationIDs(ctx, appIDs); err == nil {
			for _, dm := range doms {
				if dm.IsPrimary {
					primary[dm.ApplicationID] = dm
				}
			}
		} else {
			slog.Warn("canvasFor: list domains", "err", err)
		}
	}
	uses := map[int64]topology.AppView{}
	for _, sy := range append(d.view.Systems, d.view.Ungrouped) {
		for _, a := range sy.Apps {
			uses[a.ID] = a
		}
	}
	elem := func(key string) string { return "s-" + strings.ReplaceAll(key, ":", "-") }
	for _, a := range d.apps {
		key := topology.AppKey(a.ID)
		kind, text := templates.AppState(ctx, a.Status)
		c := templates.CanvasCard{
			Key: key, ElemID: elem(key), Glyph: "i-app", Name: a.Name,
			Sub:     i18n.T(ctx, "canvas.internal"),
			StateID: "cs-app-" + strconv.FormatInt(a.ID, 10), StateKind: kind, StateText: text,
			Nodes: running[dockerName(a.ID)], NodeIDs: runningIDs[dockerName(a.ID)],
			Href: base + "/apps/" + strconv.FormatInt(a.ID, 10),
		}
		if dm, ok := primary[a.ID]; ok && dm.Exposed {
			c.Sub, c.SubLink = dm.Host+firstCSV(dm.Paths), true
		}
		for _, k := range uses[a.ID].Uses {
			c.To = append(c.To, elem(k))
		}
		for _, k := range uses[a.ID].Detected {
			c.Detected = append(c.Detected, elem(k))
		}
		v.Cards[key] = c
	}

	// Resources.
	ldbByID := map[int64]db.ListLogicalDatabasesByEnvironmentRow{}
	for _, l := range d.ldbs {
		ldbByID[l.ID] = l
	}
	bucketByID := map[int64]db.ListS3BucketsByEnvironmentRow{}
	for _, b := range d.buckets {
		bucketByID[b.ID] = b
	}
	// A resource is shared when, in any system showing it, it is also used
	// outside that system or its server hosts more (topology.BuildEnvView).
	shared := map[string]bool{}
	for _, sy := range append(d.view.Systems, d.view.Ungrouped) {
		for _, rv := range sy.Resources {
			if rv.Shared {
				shared[rv.Key] = true
			}
		}
	}
	for _, r := range all {
		in := d.insts[r.InstanceID]
		kind, text := templates.ServerState(ctx, in.Status)
		c := templates.CanvasCard{Key: r.Key, ElemID: elem(r.Key), Glyph: engineGlyph(r.Engine), Href: r.Href,
			StateKind: kind, StateText: text, Nodes: running[in.AppName], NodeIDs: runningIDs[in.AppName], Shared: shared[r.Key]}
		id, _ := strconv.ParseInt(r.Key[strings.IndexByte(r.Key, ':')+1:], 10, 64)
		switch r.Kind {
		case "ldb":
			l := ldbByID[id]
			c.Name = l.Name
			c.Sub = i18n.Tf(ctx, "canvas.pg_on", l.InstanceName)
			c.StateID = "cs-ldb-" + strconv.FormatInt(id, 10)
		case "bucket":
			c.Name = bucketByID[id].Name
			c.Sub = i18n.Tf(ctx, "canvas.bucket_on", r.Server)
			c.Glyph = "i-bucket"
		default:
			c.Name = in.Name
			c.Sub = i18n.Tf(ctx, "canvas.server", engineLabel(r.Engine))
		}
		v.Cards[r.Key] = c
	}

	// The node filter: chips and cards match by Swarm node ID, chips show the
	// display name. Someone who may not see the cluster gets only the nodes
	// this environment's cards run on.
	count := map[string]int{}
	for _, c := range v.Cards {
		for _, id := range c.NodeIDs {
			count[id]++
		}
	}
	operator := auth.IsInstanceAdmin(ctx)
	pressure := map[string]string{} // by Swarm node ID
	if operator {
		byKey := map[string]string{}
		for _, n := range s.nodeSummaries(ctx) {
			if n.MemTotal > 0 && !n.Stale {
				if pct := int(n.MemUsed * 100 / n.MemTotal); pct >= 85 {
					byKey[n.Node] = i18n.Tf(ctx, "canvas.memory_pct", pct)
				}
			}
		}
		for id, key := range s.metricsNodeKeys(ctx, nodes) {
			if w := byKey[key]; w != "" {
				pressure[id] = w
			}
		}
	}
	for _, n := range nodes {
		if !operator && count[n.ID] == 0 {
			continue
		}
		name := nodeDisplayName(n, labels)
		v.Nodes = append(v.Nodes, templates.CanvasNode{ID: n.ID, Name: name, Count: count[n.ID], Warn: pressure[n.ID]})
	}

	if s.cloner != nil {
		if notice, err := s.q.EnvironmentCloneNotice(ctx, &env.ID); err == nil {
			v.Cloned, v.ClonePending = notice.Cloned, notice.Pending
			v.SettingsURL = base + "/settings"
		} else {
			slog.Warn("canvasFor: clone notice lookup failed", "environment_id", env.ID)
		}
	}

	// A failed deploy is called out over the canvas.
	for _, a := range d.apps {
		if a.Status != deploy.StatusError {
			continue
		}
		f := &templates.CanvasFailing{Name: a.Name, Href: base + "/apps/" + strconv.FormatInt(a.ID, 10)}
		if deps, err := s.q.ListDeploymentSummariesByApplication(ctx, a.ID); err == nil && len(deps) > 0 {
			if deps[0].FinishedAt.Valid {
				f.When = deps[0].FinishedAt.Time
			} else {
				f.When = deps[0].StartedAt
			}
		}
		v.Failing = f
		break
	}
	return v
}

func engineGlyph(engine string) string {
	switch engine {
	case "postgres":
		return "i-pg"
	case "minio":
		return "i-bucket"
	default:
		return "i-kv"
	}
}

func engineLabel(engine string) string {
	switch engine {
	case "minio":
		return "S3"
	case "dragonfly":
		return "DragonFly"
	case "valkey":
		return "Valkey"
	case "redis":
		return "Redis"
	case "postgres":
		return "Postgres"
	}
	return engine
}

func isAdminRole(role string) bool { return role == "owner" || role == "admin" }

// projectPage renders a project's environment as the canvas (the first
// environment when the path names none), with the new-app sheet on ?new=app.
func (s *Server) projectPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	envs, err := s.q.ListEnvironments(r.Context(), p.ID)
	if err != nil {
		logFrom(r).Error("projectPage: list environments", "err", err, "project_id", p.ID, "org_id", o.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var active db.Environment
	if envID, ok := pathID(r, "envID"); ok {
		for _, e := range envs {
			if e.ID == envID {
				active = e
			}
		}
		if active.ID == 0 {
			http.NotFound(w, r)
			return
		}
	} else if len(envs) > 0 {
		active = envs[0]
	}
	if active.ID == 0 {
		render(w, r, http.StatusOK, templates.ProjectNoEnv(templates.Shell{Title: p.Name, Org: o, Role: role, Tab: "projects"}, p))
		return
	}
	newApp := r.URL.Query().Get("new") == "app" && isAdminRole(role)
	body := "is-canvas"
	if newApp {
		body += " has-sheet"
	}
	shell := s.envShell(r.Context(), o, role, p, active, envs, p.Name+", "+active.Name, "canvas", body)
	d := s.loadEnvData(r.Context(), o.ID, p.ID, active)
	cv := s.canvasFor(r.Context(), o, role, p, active, d, "", newApp)
	if !newApp {
		render(w, r, http.StatusOK, templates.EnvCanvas(shell, cv))
		return
	}
	registries, err := s.q.ListRegistriesByOrg(r.Context(), o.ID)
	if err != nil {
		logFrom(r).Error("projectPage: list registries", "err", err, "org_id", o.ID)
	}
	render(w, r, http.StatusOK, templates.EnvCanvasWith(shell, cv, templates.NewAppSheet(shell, registries)))
}

// envDatabasesPage lists the environment's databases, buckets and linked
// servers; ?new=database and ?new=bucket open the create sheets.
func (s *Server) envDatabasesPage(w http.ResponseWriter, r *http.Request) {
	o, role, p, e, envs, ok := s.envChain(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	v := templates.EnvDatabasesView{}
	if isAdminRole(role) {
		switch r.URL.Query().Get("new") {
		case "database", "bucket":
			v.Sheet = r.URL.Query().Get("new")
		}
	}
	body := "page"
	if v.Sheet != "" {
		body = "page has-sheet"
	}
	shell := s.envShell(ctx, o, role, p, e, envs, i18n.Sentence(i18n.T(ctx, "env.tab_databases"))+", "+p.Name+" "+e.Name, "databases", body)
	d := s.loadEnvData(ctx, o.ID, p.ID, e)
	running, _, _, _ := s.placement(ctx)
	base := envURL(o.ID, p.ID, e.ID)
	orgBase := "/orgs/" + strconv.FormatInt(o.ID, 10)

	// Who uses each resource.
	appName := map[int64]string{}
	for _, a := range d.apps {
		appName[a.ID] = a.Name
	}
	users := map[string][]templates.UsedBy{}
	for _, sy := range append(d.view.Systems, d.view.Ungrouped) {
		for _, rv := range sy.Resources {
			for _, id := range rv.Users {
				users[rv.Key] = append(users[rv.Key], templates.UsedBy{Name: appName[id], Href: base + "/apps/" + strconv.FormatInt(id, 10) + "?tab=variables"})
			}
		}
	}
	nodeOf := func(in db.DbInstance) string {
		if n := running[in.AppName]; len(n) > 0 {
			return strings.Join(n, ", ")
		}
		return in.NodeHostname
	}
	for _, l := range d.ldbs {
		key := "ldb:" + strconv.FormatInt(l.ID, 10)
		in := d.insts[l.InstanceID]
		kind, text := templates.ServerState(ctx, l.InstanceStatus)
		row := templates.EnvResourceRow{
			Key: "ldb-" + strconv.FormatInt(l.ID, 10), Glyph: "i-pg", Name: l.Name, KindLabel: i18n.T(ctx, "envdb.kind_pg"),
			Href: base + "/databases/" + strconv.FormatInt(l.ID, 10), StateKind: kind, StateText: text,
			Server: l.InstanceName, ServerHref: orgBase + "/db-servers/" + strconv.FormatInt(l.InstanceID, 10), Node: nodeOf(in),
			Detail: i18n.Tf(ctx, "envdb.detail_pg", l.DbName, l.Username), Users: users[key],
			BackupHref: base + "/databases/" + strconv.FormatInt(l.ID, 10) + "#backups",
		}
		row.BackupKind, row.BackupText = "idle", i18n.T(ctx, "envdb.no_backups")
		if bks, err := s.q.ListBackupsByLogicalDB(ctx, l.ID); err == nil && len(bks) > 0 {
			row.BackupKind, row.BackupText = backupSummary(ctx, dbBackupConfigs(bks))
		}
		v.Rows = append(v.Rows, row)
	}
	for _, b := range d.buckets {
		key := "bucket:" + strconv.FormatInt(b.ID, 10)
		in := d.insts[b.InstanceID]
		kind, text := templates.ServerState(ctx, b.InstanceStatus)
		row := templates.EnvResourceRow{
			Key: "bucket-" + strconv.FormatInt(b.ID, 10), Glyph: "i-bucket", Name: b.Name, KindLabel: i18n.T(ctx, "envdb.kind_bucket"),
			Href: base + "/buckets/" + strconv.FormatInt(b.ID, 10), StateKind: kind, StateText: text,
			Server: b.InstanceName, ServerHref: orgBase + "/db-servers/" + strconv.FormatInt(b.InstanceID, 10), Node: nodeOf(in),
			Users: users[key], BackupHref: base + "/buckets/" + strconv.FormatInt(b.ID, 10) + "#backups",
		}
		row.BackupKind, row.BackupText = "idle", i18n.T(ctx, "envdb.no_backups")
		if bks, err := s.q.ListBucketBackupsByBucket(ctx, b.ID); err == nil && len(bks) > 0 {
			row.BackupKind, row.BackupText = backupSummary(ctx, bucketBackupConfigs(bks))
		}
		v.Rows = append(v.Rows, row)
	}
	// Whole servers the apps link to (Redis and the like).
	var instKeys []string
	for k, r := range d.resources {
		if r.Kind == "instance" {
			instKeys = append(instKeys, k)
		}
	}
	sort.Strings(instKeys)
	for _, k := range instKeys {
		r := d.resources[k]
		in := d.insts[r.InstanceID]
		kind, text := templates.ServerState(ctx, in.Status)
		v.Rows = append(v.Rows, templates.EnvResourceRow{
			Key: "inst-" + strconv.FormatInt(in.ID, 10), Glyph: engineGlyph(in.Engine), Name: in.Name,
			KindLabel: i18n.Tf(ctx, "canvas.server", engineLabel(in.Engine)), StateKind: kind, StateText: text,
			Server: in.Name, ServerHref: r.Href, Node: nodeOf(in), Users: users[k],
			BackupKind: "idle", BackupText: i18n.Tf(ctx, "envdb.no_backups_for", engineLabel(in.Engine)),
		})
	}
	if v.Sheet != "" {
		for _, in := range d.insts {
			switch in.Engine {
			case "postgres":
				v.PgInsts = append(v.PgInsts, in)
			case "minio":
				v.S3Insts = append(v.S3Insts, in)
			}
		}
		sort.Slice(v.PgInsts, func(i, j int) bool { return v.PgInsts[i].Name < v.PgInsts[j].Name })
		sort.Slice(v.S3Insts, func(i, j int) bool { return v.S3Insts[i].Name < v.S3Insts[j].Name })
	}
	render(w, r, http.StatusOK, templates.EnvDatabases(shell, v))
}

// backupSummary is a backup's state in one line.
// backupConfig is what a resource's backup summary reads of one of its
// backup configurations.
type backupConfig struct {
	Enabled bool
	Status  string    // the last run's status
	RanAt   time.Time // the last run, zero when it never ran
}

// backupSummary sums up every backup configuration of a resource: the result
// is the newest run's, whichever configuration made it (a manual run on a
// paused one included); with no failure there, the resource is paused only
// when all its configurations are, "no runs yet" when none has run, fine
// otherwise. A resource with no configuration is the caller's "no backups".
func backupSummary(ctx context.Context, cfgs []backupConfig) (string, string) {
	var newest backupConfig
	active := 0
	for _, c := range cfgs {
		if c.Enabled {
			active++
		}
		if c.RanAt.After(newest.RanAt) {
			newest = c
		}
	}
	switch {
	case !newest.RanAt.IsZero() && newest.Status == "error":
		return "err", i18n.T(ctx, "envdb.backup_failed")
	case active == 0:
		return "idle", i18n.T(ctx, "envdb.backups_paused")
	case newest.RanAt.IsZero():
		return "idle", i18n.T(ctx, "envdb.backups_no_runs")
	default:
		return "ok", i18n.T(ctx, "envdb.backups_ok")
	}
}

func ranAt(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func dbBackupConfigs(bks []db.ListBackupsByLogicalDBRow) []backupConfig {
	out := make([]backupConfig, len(bks))
	for i, b := range bks {
		out[i] = backupConfig{Enabled: b.Enabled, Status: b.LastStatus, RanAt: ranAt(b.LastRunAt)}
	}
	return out
}

func bucketBackupConfigs(bks []db.BucketBackup) []backupConfig {
	out := make([]backupConfig, len(bks))
	for i, b := range bks {
		out[i] = backupConfig{Enabled: b.Enabled, Status: b.LastStatus, RanAt: ranAt(b.LastRunAt)}
	}
	return out
}

// envSettingsPage renders the environment's settings (admins only: every
// control on it changes something).
func (s *Server) envSettingsPage(w http.ResponseWriter, r *http.Request) {
	o, role, p, e, envs, ok := s.envChain(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	shell := s.envShell(ctx, o, role, p, e, envs, i18n.Sentence(i18n.T(ctx, "envset.title"))+", "+p.Name+" "+e.Name, "settings", "page")
	d := s.loadEnvData(ctx, o.ID, p.ID, e)
	v := templates.EnvSettingsView{View: d.view, Envs: envs}
	if s.cloner != nil {
		if notice, err := s.q.EnvironmentCloneNotice(ctx, &e.ID); err == nil {
			v.Cloned, v.Pending = notice.Cloned, notice.Pending
		} else {
			logFrom(r).Error("envSettingsPage: clone notice lookup failed", "environment_id", e.ID)
		}
		if key, err := envclone.NewRequestKey(); err == nil {
			v.RequestKey = key
		} else {
			logFrom(r).Error("envSettingsPage: clone request identity generation failed", "environment_id", e.ID)
		}
	}
	render(w, r, http.StatusOK, templates.EnvSettings(shell, v))
}
