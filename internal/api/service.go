package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
)

// Service holds the agent-facing API operations (app/status/env/deploy/etc.).
// It knows nothing about HTTP or MCP: the REST adapter (Task 8) and the MCP
// adapter (Task 9) both call through these same methods and map the results
// (and the *Error values returned on failure) onto their own transport shape.
type Service struct {
	q      *db.Queries
	engine docker.Engine
	dep    *deploy.Deployer
	hub    *deploy.DeployLogHub
}

// NewService wires a Service on top of the generated queries, the Docker
// engine, and the deployer/log hub used by later tasks (deploys, logs).
// engine may be nil in tests that only exercise resolution/tenancy/env — every
// docker-touching path checks for that and falls back to stored state.
func NewService(q *db.Queries, eng docker.Engine, dep *deploy.Deployer, hub *deploy.DeployLogHub) *Service {
	return &Service{q: q, engine: eng, dep: dep, hub: hub}
}

// requireWrite is the single gate every mutating operation calls first.
func requireWrite(id Identity) error {
	if !id.CanWrite() {
		return Forbidden("this token is read-only, or its owner is no longer an admin of the organization")
	}
	return nil
}

// resolveApp accepts either a numeric application id or a
// "project/environment/app" path, and verifies the whole org→project→env→app
// chain. A mismatch is reported as not_found, never forbidden: telling the
// caller an app exists in someone else's org turns id enumeration into recon.
func (s *Service) resolveApp(ctx context.Context, id Identity, ref string) (db.Application, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return db.Application{}, Invalid("app is required: pass a numeric id or a project/environment/app path")
	}

	if numeric, err := strconv.ParseInt(ref, 10, 64); err == nil {
		chain, err := s.q.GetApplicationChain(ctx, numeric)
		if err != nil || chain.OrgID != id.OrgID {
			return db.Application{}, NotFound(fmt.Sprintf("no application %q in this organization", ref))
		}
		app, err := s.q.GetApplication(ctx, numeric)
		if err != nil {
			return db.Application{}, NotFound(fmt.Sprintf("no application %q in this organization", ref))
		}
		return app, nil
	}

	parts := strings.Split(ref, "/")
	if len(parts) != 3 {
		return db.Application{}, Invalid(fmt.Sprintf("app %q is neither a numeric id nor a project/environment/app path", ref))
	}
	apps, err := s.listAppsRaw(ctx, id)
	if err != nil {
		return db.Application{}, err
	}
	for _, a := range apps {
		if a.ProjectName == parts[0] && a.EnvName == parts[1] && a.App.Name == parts[2] {
			return a.App, nil
		}
	}
	return db.Application{}, NotFound(fmt.Sprintf("no application %q in this organization", ref))
}
