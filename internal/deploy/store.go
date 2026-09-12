package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/proshik/krill/internal/builder"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/envtext"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/traefik"
)

// DBStore implements Store on top of sqlc queries.
type DBStore struct {
	q *db.Queries
}

func NewDBStore(q *db.Queries) *DBStore { return &DBStore{q: q} }

// ErrCredentialHostMismatch guards against a credential being handed to a host
// it was not registered for: git answers an askpass prompt for ANY host, so an
// app pointed at attacker.example would otherwise hand over another admin's PAT.
var ErrCredentialHostMismatch = errors.New("credential host does not match the target host")

func checkGitCredentialHost(gitURL, credHost string) error {
	h, err := builder.HostOf(gitURL)
	if err != nil {
		return err
	}
	want := builder.NormalizeHost(credHost)
	if want == "" {
		// Fail CLOSED: an empty/unusable stored host is not "nothing to
		// check against" — it must never be treated as a pass, or a
		// credential with a broken stored host would be handed to any host.
		return fmt.Errorf("%w: stored credential host %q is empty or unusable", ErrCredentialHostMismatch, credHost)
	}
	if want != h {
		return fmt.Errorf("%w: credential is registered for %q, repository is on %q", ErrCredentialHostMismatch, want, h)
	}
	return nil
}

func checkRegistryHost(image, registryURL string) error {
	// Canonicalize both sides: Docker Hub answers to several names, and a
	// hostless image (which ImageHost maps to docker.io) paired with a stored
	// "index.docker.io" or "registry-1.docker.io" is the same registry.
	h := docker.CanonicalRegistryHost(docker.ImageHost(image))
	want := docker.CanonicalRegistryHost(builder.NormalizeHost(docker.RegistryHost(registryURL)))
	if want == "" {
		// Same fail-closed rule as checkGitCredentialHost: an unusable stored
		// registry host must reject, not silently skip the check.
		return fmt.Errorf("%w: stored registry host %q is empty or unusable", ErrCredentialHostMismatch, registryURL)
	}
	if want != h {
		return fmt.Errorf("%w: registry is %q, image is on %q", ErrCredentialHostMismatch, want, h)
	}
	return nil
}

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
	netName, err := s.q.GetOrganizationNetworkByApp(ctx, a.ID)
	if err != nil {
		return App{}, err
	}
	out.Network = netName
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
			if herr := checkRegistryHost(a.Image, reg.RegistryUrl); herr != nil {
				return App{}, herr
			}
			pw, derr := secret.Dec(reg.Password)
			if derr != nil {
				return App{}, fmt.Errorf("registry %d password: %w", reg.ID, derr)
			}
			if auth, aerr := docker.EncodeRegistryAuth(reg.Username, pw, reg.RegistryUrl); aerr == nil {
				out.RegistryAuth = auth
			}
		}
	}
	// Build-time credentials/args/secrets only matter for source builds.
	if a.SourceType == "dockerfile" {
		if a.GitCredentialID != nil {
			if gc, gerr := s.q.GetGitCredential(ctx, *a.GitCredentialID); gerr == nil {
				if herr := checkGitCredentialHost(a.GitUrl, gc.Host); herr != nil {
					return App{}, herr
				}
				tok, derr := secret.Dec(gc.Token)
				if derr != nil {
					return App{}, fmt.Errorf("git credential %d token: %w", gc.ID, derr)
				}
				out.GitAuth = &builder.GitAuth{Username: gc.Username, Token: tok}
			} else {
				slog.Warn("git-credential: not found, cloning without auth", "app", a.ID, "git_credential_id", *a.GitCredentialID)
			}
		}
		out.BuildArgs = parseEnvText(a.BuildArgs)
		bs, derr := secret.Dec(a.BuildSecrets)
		if derr != nil {
			return App{}, fmt.Errorf("app %d build secrets: %w", a.ID, derr)
		}
		out.BuildSecrets = parseEnvText(bs)
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
			Owner:  strDeref(v.Owner),
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
		val, ok, rerr := s.resolveDBLinkValue(ctx, l)
		if rerr != nil {
			return App{}, fmt.Errorf("resolve db-link %d: %w", l.ID, rerr)
		}
		if ok {
			out.Env[l.VarName] = val // linked-DB value wins over env_text on key collision
		} else {
			slog.Warn("db-link: target database missing, skipping injection", "app", a.ID, "link_id", l.ID, "var", l.VarName)
		}
	}
	return out, nil
}

// resolveDBLinkValue derives the link's source fields (password decrypted live)
// and dispatches to the target instance's engine driver for the value named by
// l.Field. Returns ("", false, nil) when the target row genuinely no longer
// exists (pgx.ErrNoRows) -- e.g. the linked logical database or instance was
// deleted -- or when the driver itself reports an unknown engine/field, so the
// caller can skip injection with just a warning. Any other error (pool
// exhaustion, deadline, connection loss, ...) is returned as-is so the caller
// fails the deploy instead of silently shipping the app without its injected
// connection string.
func (s *DBStore) resolveDBLinkValue(ctx context.Context, l db.ListDBLinksByApplicationRow) (string, bool, error) {
	switch {
	case l.LogicalDatabaseID != nil:
		ld, err := s.q.GetLogicalDatabase(ctx, *l.LogicalDatabaseID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", false, nil
			}
			return "", false, err
		}
		inst, err := s.q.GetDBInstance(ctx, ld.InstanceID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", false, nil
			}
			return "", false, err
		}
		pw, derr := secret.Dec(ld.Password)
		if derr != nil {
			return "", false, fmt.Errorf("logical database %d password: %w", ld.ID, derr)
		}
		src := drivers.LinkSource{
			AppName:   inst.AppName,
			Superuser: ld.Username,
			Password:  pw,
			DBName:    ld.DbName,
			Scheme:    l.Scheme,
		}
		val, ok := drivers.LinkValue("postgres", src, l.Field)
		return val, ok, nil
	case l.InstanceID != nil:
		inst, err := s.q.GetDBInstance(ctx, *l.InstanceID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", false, nil
			}
			return "", false, err
		}
		pw, derr := secret.Dec(inst.SuperuserPassword)
		if derr != nil {
			return "", false, fmt.Errorf("db instance %d superuser password: %w", inst.ID, derr)
		}
		src := drivers.LinkSource{
			AppName:   inst.AppName,
			Superuser: inst.Superuser,
			Password:  pw,
			Scheme:    l.Scheme,
		}
		val, ok := drivers.LinkValue(inst.Engine, src, l.Field)
		return val, ok, nil
	default:
		return "", false, nil
	}
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

// CountRunningDeployments reports how many deploys for this app are still in
// flight, so enqueue can refuse to pile a second one onto the shared queue.
func (s *DBStore) CountRunningDeployments(ctx context.Context, appID int64) (int64, error) {
	return s.q.CountRunningDeploymentsByApplication(ctx, appID)
}

// CountRunningDeploymentsByOrg reports how many deploys are still in flight
// across the whole organization that owns appID, so enqueue can refuse to let
// one tenant fill the single shared build worker's queue for everyone else.
func (s *DBStore) CountRunningDeploymentsByOrg(ctx context.Context, appID int64) (int64, error) {
	return s.q.CountRunningDeploymentsByOrg(ctx, appID)
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

// FailOrphanedDeployments marks deploys left mid-flight by a crash or a kill -9
// as failed, returning how many rows it reconciled. Call it once at startup,
// before the deploy worker runs: a process that has just booted owns no
// in-flight deploy, so every 'running' row is a leftover that would otherwise
// spin in the history forever.
func (s *DBStore) FailOrphanedDeployments(ctx context.Context) (int64, error) {
	return s.q.FailOrphanedDeployments(ctx)
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

// parseEnvText derives the deploy-time environment from env_text. Duplicate
// keys are not an error here — the deploy must produce an environment either
// way, and the form save already refuses to store a file containing them.
func parseEnvText(raw string) map[string]string {
	env, _ := envtext.Map(raw)
	return env
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
