package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	prov   Provisioner // creating operations; nil answers unavailable

	// Optional collaborators the server wires (SetAPI); each one missing
	// leaves its field out of an answer or its operation unavailable.
	instanceURL     func(context.Context) string
	stopping        context.Context // cancelled when Krill starts shutting down
	metricsInterval time.Duration
	memDefault      string // instance-wide default limits a deploy falls back to
	cpuDefault      string
	backups         Backups
	gateway         Gateway
	domains         domainProbe
}

// NewService wires a Service on top of the generated queries, the Docker
// engine and the deployer. Deploy logs are read from the deployments table,
// not from the live log hub, so the hub is deliberately not a dependency.
// engine may be nil in tests that only exercise resolution/tenancy/env — every
// docker-touching path checks for that and falls back to stored state.
func NewService(q *db.Queries, eng docker.Engine, dep *deploy.Deployer) *Service {
	return &Service{q: q, engine: eng, dep: dep, domains: systemDomainProbe()}
}

// SetInstanceURL wires the installation's externally reachable base URL (the
// one webhook addresses use), reported by Whoami.
func (s *Service) SetInstanceURL(fn func(context.Context) string) { s.instanceURL = fn }

// SetStopping wires a context cancelled when Krill starts shutting down: a
// call waiting for a deployment or a log line ends then with what it has. An
// MCP tool runs on its session's context, which a shutdown does not end, so
// the request context alone is not enough.
func (s *Service) SetStopping(ctx context.Context) { s.stopping = ctx }

// SetMetricsInterval tells AppStatus how often the monitoring sampler
// records, so it can tell a current usage sample from a stale one.
func (s *Service) SetMetricsInterval(d time.Duration) { s.metricsInterval = d }

// SetResourceDefaults wires the instance-wide memory and CPU limits a deploy
// applies to an app without its own (KRILL_DEFAULT_MEMORY_LIMIT/_CPU_LIMIT).
func (s *Service) SetResourceDefaults(mem, cpu string) {
	s.memDefault, s.cpuDefault = strings.TrimSpace(mem), strings.TrimSpace(cpu)
}

// SetBackups wires the backup services ListBackups and BackupNow use.
func (s *Service) SetBackups(b Backups) { s.backups = b }

// SetGateway wires what DomainStatus knows about Krill's own gateway.
func (s *Service) SetGateway(g Gateway) { s.gateway = g }

// waitDone is closed when a long wait must end early: Krill is shutting
// down. Nil (never closes) when no stopping context is wired.
func (s *Service) waitDone() <-chan struct{} {
	if s.stopping == nil {
		return nil
	}
	return s.stopping.Done()
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
