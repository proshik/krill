package server

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
)

// orgIndex is one organization's projects, environments and apps, read in
// three queries, for pages that cross-link resources across the whole
// organization ("used by", "holds", the palette). Every row is the
// organization's own.
// indexApp is what the index keeps of an app: no env or build values.
type indexApp = db.ListAppIndexByEnvironmentIDsRow

type orgIndex struct {
	orgID    int64
	projects []db.ListProjectsWithCountsRow
	projName map[int64]string
	envs     map[int64]db.Environment
	apps     []indexApp
	appByID  map[int64]indexApp
}

func (s *Server) loadOrgIndex(ctx context.Context, orgID int64) (orgIndex, error) {
	ix := orgIndex{orgID: orgID, projName: map[int64]string{}, envs: map[int64]db.Environment{}, appByID: map[int64]indexApp{}}
	projects, err := s.q.ListProjectsWithCounts(ctx, orgID)
	if err != nil {
		return ix, fmt.Errorf("list projects: %w", err)
	}
	ix.projects = projects
	ids := make([]int64, 0, len(projects))
	for _, p := range projects {
		ix.projName[p.ID] = p.Name
		ids = append(ids, p.ID)
	}
	if len(ids) == 0 {
		return ix, nil
	}
	envs, err := s.q.ListEnvironmentsByProjectIDs(ctx, ids)
	if err != nil {
		return ix, fmt.Errorf("list environments: %w", err)
	}
	envIDs := make([]int64, 0, len(envs))
	for _, e := range envs {
		ix.envs[e.ID] = e
		envIDs = append(envIDs, e.ID)
	}
	if len(envIDs) == 0 {
		return ix, nil
	}
	apps, err := s.q.ListAppIndexByEnvironmentIDs(ctx, envIDs)
	if err != nil {
		return ix, fmt.Errorf("list applications: %w", err)
	}
	ix.apps = apps
	for _, a := range apps {
		ix.appByID[a.ID] = a
	}
	return ix, nil
}

// envLabel is "project environment" for an environment of the organization.
func (ix orgIndex) envLabel(envID int64) string {
	e, ok := ix.envs[envID]
	if !ok {
		return ""
	}
	return ix.projName[e.ProjectID] + " " + e.Name
}

func (ix orgIndex) envURL(envID int64) string {
	e := ix.envs[envID]
	return envURL(ix.orgID, e.ProjectID, e.ID)
}

func (ix orgIndex) appURL(a indexApp) string {
	return ix.envURL(a.EnvironmentID) + "/apps/" + strconv.FormatInt(a.ID, 10)
}

func (ix orgIndex) appIDs() []int64 {
	ids := make([]int64, 0, len(ix.apps))
	for _, a := range ix.apps {
		ids = append(ids, a.ID)
	}
	return ids
}

// liveIndexStatuses replaces the given apps' stored statuses with Swarm's
// view, as deriveCardStatuses does for full rows. Only the services of these
// apps are asked for, and nothing at all for none.
func (s *Server) liveIndexStatuses(ctx context.Context, apps []indexApp) {
	if s.engine == nil || len(apps) == 0 {
		return
	}
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = dockerName(a.ID)
	}
	states, err := s.engine.ServiceStates(ctx, names)
	if err != nil {
		slog.Error("liveIndexStatuses: service states", "err", err)
		return
	}
	for i := range apps {
		apps[i].Status = deploy.DeriveStatus(states[dockerName(apps[i].ID)], apps[i].Status)
	}
}
