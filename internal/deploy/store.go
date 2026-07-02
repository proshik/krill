package deploy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/proshik/krill/internal/builder"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
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
		Env:            parseEnvText(a.EnvText),
		SourceType:     a.SourceType,
		GitURL:         a.GitUrl,
		GitBranch:      a.GitBranch,
		DockerfilePath: a.DockerfilePath,
		Args:           docker.SplitCommand(strDeref(a.Command)),
	}
	for _, d := range doms {
		out.Domains = append(out.Domains, traefik.Domain{
			Host: d.Host, TLS: d.Tls, Exposed: d.Exposed, Paths: traefik.SplitPaths(d.Paths),
			BasicAuthUsers: traefik.SplitPaths(d.BasicAuthUsers), AllowedIPs: traefik.SplitPaths(d.AllowedIps),
		})
	}
	out.Replicas = uint64(a.Replicas)
	out.RestartCondition = a.RestartCondition
	out.RestartMaxAttempts = uint64(a.RestartMaxAttempts)
	out.PlacementMode = a.PlacementMode
	out.PlacementNodes = splitCSV(a.PlacementNodes)
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
			if auth, aerr := docker.EncodeRegistryAuth(reg.Username, secret.Dec(reg.Password), reg.RegistryUrl); aerr == nil {
				out.RegistryAuth = auth
			}
		}
	}
	// Build-time credentials/args/secrets only matter for source builds.
	if a.SourceType == "dockerfile" {
		if a.GitCredentialID != nil {
			if gc, gerr := s.q.GetGitCredential(ctx, *a.GitCredentialID); gerr == nil {
				out.GitAuth = &builder.GitAuth{Username: gc.Username, Token: secret.Dec(gc.Token)}
			} else {
				slog.Warn("git-credential: not found, cloning without auth", "app", a.ID, "git_credential_id", *a.GitCredentialID)
			}
		}
		out.BuildArgs = parseEnvText(a.BuildArgs)
		out.BuildSecrets = parseEnvText(secret.Dec(a.BuildSecrets))
	}
	vols, verr := s.q.ListVolumesByApplication(ctx, a.ID)
	if verr != nil {
		return App{}, verr
	}
	for _, v := range vols {
		out.Mounts = append(out.Mounts, docker.MountSpec{
			Type:   "volume",
			Source: docker.VolumeName(a.ID, v.Name),
			Target: v.MountPath,
		})
	}
	ports, perr := s.q.ListAppPorts(ctx, a.ID)
	if perr != nil {
		return App{}, perr
	}
	for _, p := range ports {
		out.Ports = append(out.Ports, docker.PortSpec{
			Target:    uint32(p.ContainerPort),
			Published: uint32(p.HostPort),
			Mode:      "host",
			UDP:       p.Protocol == "udp",
		})
	}
	links, lerr := s.q.ListDBLinksByApplication(ctx, a.ID)
	if lerr != nil {
		return App{}, lerr
	}
	for _, l := range links {
		if url, ok := s.resolveDBLinkURL(ctx, l); ok {
			out.Env[l.VarName] = url // linked-DB value wins over env_text on key collision
		} else {
			slog.Warn("db-link: target database missing, skipping injection", "app", a.ID, "link_id", l.ID, "var", l.VarName)
		}
	}
	return out, nil
}

// resolveDBLinkURL builds the internal connection URL for a link (password
// decrypted live, never stored in env_text). Postgres links point at a logical
// database inside an instance; redis links point at the instance itself.
// Returns false if the target no longer exists.
func (s *DBStore) resolveDBLinkURL(ctx context.Context, l db.ListDBLinksByApplicationRow) (string, bool) {
	switch {
	case l.LogicalDatabaseID != nil:
		ld, err := s.q.GetLogicalDatabase(ctx, *l.LogicalDatabaseID)
		if err != nil {
			return "", false
		}
		inst, err := s.q.GetDBInstance(ctx, ld.InstanceID)
		if err != nil {
			return "", false
		}
		return l.Scheme + "://" + ld.Username + ":" + secret.Dec(ld.Password) + "@" + inst.AppName + ":5432/" + ld.DbName, true
	case l.InstanceID != nil:
		inst, err := s.q.GetDBInstance(ctx, *l.InstanceID)
		if err != nil {
			return "", false
		}
		return "redis://default:" + secret.Dec(inst.SuperuserPassword) + "@" + inst.AppName + ":6379", true
	}
	return "", false
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

// parseEnvText turns the stored raw KEY=VALUE lines into a map for the deploy
// spec (env is a set there — order is irrelevant). The editor keeps the raw
// order-preserving text; this is the single derivation for the container env.
// splitCSV splits a comma-separated string into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseEnvText(raw string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k = strings.TrimSpace(k); k != "" {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
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
