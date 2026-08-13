package api

import (
	"context"
	"fmt"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/envtext"
)

// WhoamiResult reports the caller's own resolved identity: which user and
// organization the token maps to, and what it is allowed to do.
type WhoamiResult struct {
	UserID   int64  `json:"user_id"`
	OrgID    int64  `json:"org_id"`
	OrgName  string `json:"org_name"`
	Level    string `json:"level"`
	Role     string `json:"role"`
	CanWrite bool   `json:"can_write"`
}

// AppSummary is the list-view shape of one application.
type AppSummary struct {
	ID         int64    `json:"id"`
	Path       string   `json:"path"` // project/env/app — what agents should pass back
	Name       string   `json:"name"`
	SourceType string   `json:"source_type"` // image | dockerfile
	Image      string   `json:"image,omitempty"`
	Status     string   `json:"status"` // idle | deploying | running | error
	Domains    []string `json:"domains,omitempty"`
}

// AppStatus is the detail-view shape: the summary plus live/last-deploy facts.
type AppStatus struct {
	AppSummary
	Replicas       string `json:"replicas"` // "1/1"
	Node           string `json:"node,omitempty"`
	LastDeployID   int64  `json:"last_deploy_id,omitempty"`
	LastDeployAt   string `json:"last_deploy_at,omitempty"` // RFC3339
	LastDeployStat string `json:"last_deploy_status,omitempty"`
}

// EnvKey names one environment variable without ever carrying its value.
type EnvKey struct {
	Key    string `json:"key"`
	Source string `json:"source"` // "literal" | "db-link"
}

// Whoami reports the caller's own resolved identity.
func (s *Service) Whoami(ctx context.Context, id Identity) (WhoamiResult, error) {
	org, err := s.q.GetOrganization(ctx, id.OrgID)
	if err != nil {
		return WhoamiResult{}, fmt.Errorf("whoami: get organization: %w", err)
	}
	return WhoamiResult{
		UserID:   id.UserID,
		OrgID:    id.OrgID,
		OrgName:  org.Name,
		Level:    string(id.Level),
		Role:     id.Role.String(),
		CanWrite: id.CanWrite(),
	}, nil
}

// appRow is one application together with the project/environment names that
// make up its display path. listAppsRaw assembles these batched, so both
// resolveApp's path lookup and ListApps share the same O(1)-query walk of the
// org instead of querying per project/environment.
type appRow struct {
	App         db.Application
	ProjectName string
	EnvName     string
}

// listAppsRaw walks org -> projects -> environments -> applications in three
// batched queries total (ListProjects, then ListEnvironmentsByProjectIDs, then
// ListApplicationsByEnvironmentIDs) — never one query per project or
// environment, regardless of how many the org has.
func (s *Service) listAppsRaw(ctx context.Context, id Identity) ([]appRow, error) {
	projects, err := s.q.ListProjects(ctx, id.OrgID)
	if err != nil {
		return nil, fmt.Errorf("list apps: list projects: %w", err)
	}
	if len(projects) == 0 {
		return nil, nil
	}
	projName := make(map[int64]string, len(projects))
	projIDs := make([]int64, 0, len(projects))
	for _, p := range projects {
		projName[p.ID] = p.Name
		projIDs = append(projIDs, p.ID)
	}

	envs, err := s.q.ListEnvironmentsByProjectIDs(ctx, projIDs)
	if err != nil {
		return nil, fmt.Errorf("list apps: list environments: %w", err)
	}
	if len(envs) == 0 {
		return nil, nil
	}
	type envMeta struct{ name, proj string }
	meta := make(map[int64]envMeta, len(envs))
	envIDs := make([]int64, 0, len(envs))
	for _, e := range envs {
		meta[e.ID] = envMeta{name: e.Name, proj: projName[e.ProjectID]}
		envIDs = append(envIDs, e.ID)
	}

	apps, err := s.q.ListApplicationsByEnvironmentIDs(ctx, envIDs)
	if err != nil {
		return nil, fmt.Errorf("list apps: list applications: %w", err)
	}
	out := make([]appRow, 0, len(apps))
	for _, a := range apps {
		m := meta[a.EnvironmentID]
		out = append(out, appRow{App: a, ProjectName: m.proj, EnvName: m.name})
	}
	return out, nil
}

// ListApps lists every application in the caller's organization. Status comes
// from one bulk docker.Engine.ServiceStates call, not one per app; domains
// come from one bulk ListDomainsByApplicationIDs call, not one per app. When
// the engine is nil (unit tests, or docker unreachable) states stays nil, and
// a lookup miss on a nil map yields the zero ServiceState{Found: false} —
// which deploy.DeriveStatus already treats as "fall back to the stored
// status", so no separate nil-engine branch is needed at the call site below.
func (s *Service) ListApps(ctx context.Context, id Identity) ([]AppSummary, error) {
	rows, err := s.listAppsRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]AppSummary, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}

	appIDs := make([]int64, 0, len(rows))
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		appIDs = append(appIDs, r.App.ID)
		names = append(names, docker.ServiceName(r.App.ID))
	}

	domRows, err := s.q.ListDomainsByApplicationIDs(ctx, appIDs)
	if err != nil {
		return nil, fmt.Errorf("list apps: list domains: %w", err)
	}
	domsByApp := make(map[int64][]string, len(appIDs))
	for _, d := range domRows {
		domsByApp[d.ApplicationID] = append(domsByApp[d.ApplicationID], d.Host)
	}

	var states map[string]docker.ServiceState
	if s.engine != nil {
		states, err = s.engine.ServiceStates(ctx, names)
		if err != nil {
			return nil, fmt.Errorf("list apps: service states: %w", err)
		}
	}

	for _, r := range rows {
		a := r.App
		out = append(out, AppSummary{
			ID:         a.ID,
			Path:       r.ProjectName + "/" + r.EnvName + "/" + a.Name,
			Name:       a.Name,
			SourceType: a.SourceType,
			Image:      a.Image,
			Status:     deploy.DeriveStatus(states[docker.ServiceName(a.ID)], a.Status),
			Domains:    domsByApp[a.ID],
		})
	}
	return out, nil
}

// appPath resolves an application's "project/environment/app" display path
// with two point lookups. Used only by single-app operations, where two extra
// queries per call is not the fan-out concern that a list endpoint is.
func (s *Service) appPath(ctx context.Context, app db.Application) (string, error) {
	env, err := s.q.GetEnvironment(ctx, app.EnvironmentID)
	if err != nil {
		return "", fmt.Errorf("app path: get environment: %w", err)
	}
	proj, err := s.q.GetProject(ctx, env.ProjectID)
	if err != nil {
		return "", fmt.Errorf("app path: get project: %w", err)
	}
	return proj.Name + "/" + env.Name + "/" + app.Name, nil
}

// AppStatus reports one application's live status: docker state and node
// placement when the engine is available, the stored status otherwise, plus
// its most recent deployment.
func (s *Service) AppStatus(ctx context.Context, id Identity, ref string) (AppStatus, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return AppStatus{}, err
	}
	path, err := s.appPath(ctx, app)
	if err != nil {
		return AppStatus{}, err
	}
	doms, err := s.q.ListDomainsByApplication(ctx, app.ID)
	if err != nil {
		return AppStatus{}, fmt.Errorf("app status: list domains: %w", err)
	}
	domains := make([]string, 0, len(doms))
	for _, d := range doms {
		domains = append(domains, d.Host)
	}

	var state docker.ServiceState
	var node string
	if s.engine != nil {
		name := docker.ServiceName(app.ID)
		states, serr := s.engine.ServiceStates(ctx, []string{name})
		if serr != nil {
			return AppStatus{}, fmt.Errorf("app status: service states: %w", serr)
		}
		state = states[name]
		// Node placement is supplementary detail: best-effort, not worth failing
		// the whole status call over if it errors.
		if tasks, terr := s.engine.ServiceTasks(ctx, name); terr == nil {
			for _, t := range tasks {
				if t.State == "running" {
					node = t.NodeName
					break
				}
			}
		}
	}

	// state.Found is false whenever the engine is nil or the service was never
	// deployed — the running count is then simply unknown, not zero, so the
	// desired half (known from the stored column) is shown alone rather than
	// implying "0 running" as "?/N" would not.
	replicas := fmt.Sprintf("?/%d", app.Replicas)
	if state.Found {
		replicas = fmt.Sprintf("%d/%d", state.Running, state.Desired)
	}

	out := AppStatus{
		AppSummary: AppSummary{
			ID:         app.ID,
			Path:       path,
			Name:       app.Name,
			SourceType: app.SourceType,
			Image:      app.Image,
			Status:     deploy.DeriveStatus(state, app.Status),
			Domains:    domains,
		},
		Replicas: replicas,
		Node:     node,
	}

	deploys, err := s.q.ListDeploymentsByApplication(ctx, app.ID)
	if err != nil {
		return AppStatus{}, fmt.Errorf("app status: list deployments: %w", err)
	}
	if len(deploys) > 0 {
		last := deploys[0] // ORDER BY started_at DESC — most recent first
		out.LastDeployID = last.ID
		out.LastDeployAt = last.StartedAt.Format(time.RFC3339)
		out.LastDeployStat = last.Status
	}
	return out, nil
}

// ListEnv lists an application's environment variable names — never values.
// Keys sourced from env_text are "literal". Keys injected by a DB link at
// deploy time are "db-link": a DB link overrides the same key in env_text
// (deploy.parseEnvText applies it last) and can also inject a key that is not
// in env_text at all, since its value is never stored as literal text.
func (s *Service) ListEnv(ctx context.Context, id Identity, ref string) ([]EnvKey, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return nil, err
	}
	links, err := s.q.ListDBLinksByApplication(ctx, app.ID)
	if err != nil {
		return nil, fmt.Errorf("list env: list db links: %w", err)
	}
	fromLink := make(map[string]bool, len(links))
	for _, l := range links {
		fromLink[l.VarName] = true
	}

	out := make([]EnvKey, 0, len(links)+4)
	for _, k := range envtext.Keys(app.EnvText) {
		source := "literal"
		if fromLink[k] {
			source = "db-link"
			delete(fromLink, k)
		}
		out = append(out, EnvKey{Key: k, Source: source})
	}
	// DB-link vars that inject a key not present in env_text at all.
	for _, l := range links {
		if fromLink[l.VarName] {
			out = append(out, EnvKey{Key: l.VarName, Source: "db-link"})
			delete(fromLink, l.VarName)
		}
	}
	return out, nil
}
