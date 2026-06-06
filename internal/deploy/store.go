package deploy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/traefik"
)

// DBStore implements Store on top of sqlc queries.
type DBStore struct {
	q *db.Queries
}

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

func (s *DBStore) GetApplication(ctx context.Context, id int64) (App, error) {
	a, err := s.q.GetApplication(ctx, id)
	if err != nil {
		return App{}, err
	}
	doms, derr := s.q.ListDomainsByApplication(ctx, a.ID)
	if derr != nil {
		return App{}, derr
	}
	out := App{
		ID:             a.ID,
		Name:           a.Name,
		Image:          a.Image,
		Tag:            a.Tag,
		Domain:         a.Domain,
		Port:           a.Port,
		Env:            a.Env,
		SourceType:     a.SourceType,
		GitURL:         a.GitUrl,
		GitBranch:      a.GitBranch,
		DockerfilePath: a.DockerfilePath,
		Args:           docker.SplitCommand(strDeref(a.Command)),
	}
	for _, d := range doms {
		out.Domains = append(out.Domains, traefik.Domain{
			Host: d.Host, TLS: d.Tls, Exposed: d.Exposed, Paths: traefik.SplitPaths(d.Paths),
		})
	}
	out.Replicas = uint64(a.Replicas)
	out.RestartCondition = a.RestartCondition
	out.RestartMaxAttempts = uint64(a.RestartMaxAttempts)
	if mb, err := docker.ParseMemoryBytes(strDeref(a.MemoryLimit)); err == nil {
		out.MemoryLimitBytes = mb
	} else {
		slog.Warn("advanced: invalid stored memory_limit", "app", a.ID, "value", strDeref(a.MemoryLimit), "err", err)
	}
	if nc, err := docker.ParseNanoCPUs(strDeref(a.CpuLimit)); err == nil {
		out.NanoCPUs = nc
	} else {
		slog.Warn("advanced: invalid stored cpu_limit", "app", a.ID, "value", strDeref(a.CpuLimit), "err", err)
	}
	out.Healthcheck = buildHealthcheck(a)
	if a.RegistryID != nil {
		if reg, rerr := s.q.GetRegistry(ctx, *a.RegistryID); rerr == nil {
			if auth, aerr := docker.EncodeRegistryAuth(reg.Username, reg.Password, reg.RegistryUrl); aerr == nil {
				out.RegistryAuth = auth
			}
		}
	}
	return out, nil
}

func (s *DBStore) SetStatus(ctx context.Context, id int64, status string) error {
	return s.q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: id, Status: status})
}

func (s *DBStore) CreateDeployment(ctx context.Context, appID int64, trigger string) (int64, error) {
	d, err := s.q.CreateDeployment(ctx, db.CreateDeploymentParams{ApplicationID: appID, Trigger: trigger})
	if err != nil {
		return 0, err
	}
	return d.ID, nil
}

func (s *DBStore) FinishDeployment(ctx context.Context, deployID int64, status, imageTag, errMsg, log string) error {
	return s.q.FinishDeployment(ctx, db.FinishDeploymentParams{
		ID: deployID, Status: status, ImageTag: imageTag, ErrorMessage: errMsg, Log: log,
	})
}

func (s *DBStore) GetDeploymentApp(ctx context.Context, deployID int64) (App, error) {
	dep, err := s.q.GetDeployment(ctx, deployID)
	if err != nil {
		return App{}, err
	}
	return s.GetApplication(ctx, dep.ApplicationID)
}

func (s *DBStore) ClearOldDeploymentLogs(ctx context.Context) error {
	return s.q.ClearOldDeploymentLogs(ctx)
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// buildHealthcheck assembles a HealthcheckSpec from the app's healthcheck
// columns. Returns nil when no command is configured (healthcheck disabled).
func buildHealthcheck(a db.Application) *docker.HealthcheckSpec {
	cmd := strings.TrimSpace(strDeref(a.HealthcheckCmd))
	if cmd == "" {
		return nil
	}
	hc := &docker.HealthcheckSpec{Test: []string{"CMD-SHELL", cmd}}
	if d, err := time.ParseDuration(strDeref(a.HealthcheckInterval)); err == nil {
		hc.Interval = d
	}
	if d, err := time.ParseDuration(strDeref(a.HealthcheckTimeout)); err == nil {
		hc.Timeout = d
	}
	if d, err := time.ParseDuration(strDeref(a.HealthcheckStartPeriod)); err == nil {
		hc.StartPeriod = d
	}
	if a.HealthcheckRetries != nil {
		hc.Retries = int(*a.HealthcheckRetries)
	}
	return hc
}
