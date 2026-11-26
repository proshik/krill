package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/envtext"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/traefik"
)

// AppConfig is an application's saved settings without anything secret: no
// env values, no commands (a command or a healthcheck can carry a literal
// credential), no webhook secret or metrics token, no registry or git
// credentials, no basic-auth users.
type AppConfig struct {
	ID          int64           `json:"id"`
	Path        string          `json:"path"`
	Source      ConfigSource    `json:"source"`
	Resources   ConfigResources `json:"resources"`
	Healthcheck ConfigHealth    `json:"healthcheck"`
	Port        int32           `json:"port"`
	Ports       []ConfigPort    `json:"ports"`
	Volumes     []ConfigVolume  `json:"volumes"`
	Domains     []ConfigDomain  `json:"domains"`
	Links       []ConfigLink    `json:"links"`
	Pending     ConfigPending   `json:"pending"`
}

// ConfigSource is where the app's image comes from.
type ConfigSource struct {
	Type     string `json:"type"` // image | dockerfile
	Image    string `json:"image,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Registry string `json:"registry,omitempty"` // name of the private registry, if any
	GitURL   string `json:"git_url,omitempty"`  // without credentials
	Branch   string `json:"git_branch,omitempty"`
	// Dockerfile is the path the builder builds (default Dockerfile).
	Dockerfile string `json:"dockerfile_path,omitempty"`
	// BuildArgs and BuildSecrets are names only.
	BuildArgs    []string `json:"build_args,omitempty"`
	BuildSecrets []string `json:"build_secrets,omitempty"`
}

// ConfigResources is the app's size and placement. A limit's source says
// whether the app sets it (explicit), takes the instance default (default)
// or has none (none). Krill sets no memory or CPU reservations.
type ConfigResources struct {
	Replicas           int32  `json:"replicas"`
	MemoryLimit        string `json:"memory_limit,omitempty"`
	MemoryLimitSource  string `json:"memory_limit_source"`
	CPULimit           string `json:"cpu_limit,omitempty"`
	CPULimitSource     string `json:"cpu_limit_source"`
	RestartCondition   string `json:"restart_condition"`
	RestartMaxAttempts int32  `json:"restart_max_attempts"`
	Placement          string `json:"placement"` // any | pin | global
	// PlacementNodes are the nodes a pinned app may run on: a worker by its
	// name, any other node by its Swarm id.
	PlacementNodes []string `json:"placement_nodes,omitempty"`
}

// ConfigHealth is the app's healthcheck, without its command.
type ConfigHealth struct {
	Configured  bool   `json:"configured"`
	Interval    string `json:"interval,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Retries     int32  `json:"retries,omitempty"`
	StartPeriod string `json:"start_period,omitempty"`
}

// ConfigPort is a raw published port.
type ConfigPort struct {
	HostPort      int32  `json:"host_port"`
	ContainerPort int32  `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// ConfigVolume is a named volume and where it is mounted.
type ConfigVolume struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
}

// ConfigDomain is one domain of the app. Protection names the kinds on, never
// their users, hashes or address lists.
type ConfigDomain struct {
	Host              string   `json:"host"`
	Primary           bool     `json:"primary"`
	Exposed           bool     `json:"exposed"`
	TLS               bool     `json:"tls"`
	Paths             []string `json:"paths,omitempty"`
	RedirectToPrimary bool     `json:"redirect_to_primary"`
	Protection        []string `json:"protection,omitempty"` // basic_auth, ip_allowlist
}

// ConfigLink is a variable a deploy fills from a database, server or bucket.
type ConfigLink struct {
	Var    string `json:"var"`
	Kind   string `json:"kind"` // database | server | bucket
	Target string `json:"target"`
	Server string `json:"server,omitempty"`
	Field  string `json:"field,omitempty"`
}

// ConfigPending lists the settings sections saved but not applied yet: they
// differ from what the latest successful deploy applied (the comparison the
// web UI's "Apply settings" makes). Known is false when there is nothing to
// compare with: no deploy recorded what it applied, a deploy is in flight,
// or the settings could not be read.
type ConfigPending struct {
	Known    bool     `json:"known"`
	Sections []string `json:"sections"`
}

// AppConfig reports an application's saved settings, never a secret.
func (s *Service) AppConfig(ctx context.Context, id Identity, ref string) (AppConfig, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return AppConfig{}, err
	}
	p, err := s.appPath(ctx, app)
	if err != nil {
		return AppConfig{}, err
	}
	out := AppConfig{
		ID:          app.ID,
		Path:        p,
		Resources:   s.configResources(app),
		Healthcheck: configHealth(app),
		Port:        app.Port,
		Ports:       []ConfigPort{},
		Volumes:     []ConfigVolume{},
		Domains:     []ConfigDomain{},
		Links:       []ConfigLink{},
		Pending:     ConfigPending{Sections: []string{}},
	}
	if len(out.Resources.PlacementNodes) > 0 {
		// Stored as Swarm node ids; a worker is shown by its Krill name.
		if names, err := s.samplerNodeNames(ctx); err == nil {
			for i, id := range out.Resources.PlacementNodes {
				if n := names[id]; n != "" {
					out.Resources.PlacementNodes[i] = n
				}
			}
		}
	}
	if out.Source, err = s.configSource(ctx, app); err != nil {
		return AppConfig{}, err
	}

	ports, err := s.q.ListAppPorts(ctx, app.ID)
	if err != nil {
		return AppConfig{}, fmt.Errorf("app config: list ports: %w", err)
	}
	for _, pt := range ports {
		out.Ports = append(out.Ports, ConfigPort{HostPort: pt.HostPort, ContainerPort: pt.ContainerPort, Protocol: pt.Protocol})
	}
	vols, err := s.q.ListVolumesByApplication(ctx, app.ID)
	if err != nil {
		return AppConfig{}, fmt.Errorf("app config: list volumes: %w", err)
	}
	for _, v := range vols {
		out.Volumes = append(out.Volumes, ConfigVolume{Name: v.Name, MountPath: v.MountPath})
	}
	doms, err := s.q.ListDomainsByApplication(ctx, app.ID)
	if err != nil {
		return AppConfig{}, fmt.Errorf("app config: list domains: %w", err)
	}
	for _, d := range doms {
		out.Domains = append(out.Domains, configDomain(d))
	}
	if out.Links, err = s.configLinks(ctx, app.ID); err != nil {
		return AppConfig{}, err
	}
	if s.dep != nil {
		if p, ok := s.dep.PendingChanges(ctx, []int64{app.ID})[app.ID]; ok {
			out.Pending.Known = true
			out.Pending.Sections = append(out.Pending.Sections, p.Changed...)
		}
	}
	return out, nil
}

// withoutCredentials drops the userinfo of a git URL. An scp-like address
// (user:token@host:org/repo) is not a URL to the parser; its part before the
// last "@" goes as well.
func withoutCredentials(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		u.User = nil
		return u.String()
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 && !strings.Contains(raw[:i], "/") {
		return raw[i+1:]
	}
	return raw
}

func (s *Service) configSource(ctx context.Context, app db.Application) (ConfigSource, error) {
	src := ConfigSource{Type: app.SourceType}
	if app.RegistryID != nil {
		reg, err := s.q.GetRegistry(ctx, *app.RegistryID)
		if err != nil {
			return ConfigSource{}, fmt.Errorf("app config: get registry: %w", err)
		}
		src.Registry = reg.Name
	}
	if app.SourceType != "dockerfile" {
		src.Image, src.Tag = app.Image, app.Tag
		return src, nil
	}
	// The builder's defaults: an empty branch clones main, an empty path
	// builds ./Dockerfile (as deploy.configSections reads them).
	src.GitURL, src.Branch, src.Dockerfile = withoutCredentials(app.GitUrl), app.GitBranch, app.DockerfilePath
	if src.Branch == "" {
		src.Branch = "main"
	}
	if src.Dockerfile == "" {
		src.Dockerfile = "Dockerfile"
	}
	src.Dockerfile = path.Clean(src.Dockerfile)
	src.BuildArgs = envtext.Keys(app.BuildArgs)
	if app.BuildSecrets != "" {
		plain, err := secret.Dec(app.BuildSecrets)
		if err != nil {
			// The names are not worth failing the whole answer over.
			slog.Warn("app config: build secrets undecryptable, their names are left out", "app_id", app.ID, "err", err)
		} else {
			src.BuildSecrets = envtext.Keys(plain)
		}
	}
	return src, nil
}

func (s *Service) configResources(app db.Application) ConfigResources {
	r := ConfigResources{
		Replicas:           app.Replicas,
		RestartCondition:   app.RestartCondition,
		RestartMaxAttempts: app.RestartMaxAttempts,
		Placement:          app.PlacementMode,
	}
	if r.RestartCondition == "" {
		r.RestartCondition = "any"
	}
	r.MemoryLimit, r.MemoryLimitSource = limitOf(app.MemoryLimit, s.memDefault)
	r.CPULimit, r.CPULimitSource = limitOf(app.CpuLimit, s.cpuDefault)
	for _, n := range strings.Split(app.PlacementNodes, ",") {
		if n = strings.TrimSpace(n); n != "" {
			r.PlacementNodes = append(r.PlacementNodes, n)
		}
	}
	return r
}

// limitOf is the limit a deploy applies: the app's own, else the instance
// default, else none.
func limitOf(own *string, def string) (string, string) {
	switch {
	case own != nil && strings.TrimSpace(*own) != "":
		return strings.TrimSpace(*own), "explicit"
	case def != "":
		return def, "default"
	}
	return "", "none"
}

func configHealth(app db.Application) ConfigHealth {
	h := ConfigHealth{Configured: app.HealthcheckCmd != nil && strings.TrimSpace(*app.HealthcheckCmd) != ""}
	if !h.Configured {
		return h
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	h.Interval, h.Timeout, h.StartPeriod = deref(app.HealthcheckInterval), deref(app.HealthcheckTimeout), deref(app.HealthcheckStartPeriod)
	if app.HealthcheckRetries != nil {
		h.Retries = *app.HealthcheckRetries
	}
	return h
}

func configDomain(d db.Domain) ConfigDomain {
	out := ConfigDomain{
		Host: d.Host, Primary: d.IsPrimary, Exposed: d.Exposed, TLS: d.Tls,
		Paths: traefik.SplitPaths(d.Paths), RedirectToPrimary: d.RedirectToPrimary,
	}
	if strings.TrimSpace(d.BasicAuthUsers) != "" {
		out.Protection = append(out.Protection, "basic_auth")
	}
	if strings.TrimSpace(d.AllowedIps) != "" {
		out.Protection = append(out.Protection, "ip_allowlist")
	}
	return out
}

// configLinks lists the app's database, server and bucket links by variable.
func (s *Service) configLinks(ctx context.Context, appID int64) ([]ConfigLink, error) {
	out := []ConfigLink{}
	links, err := s.q.ListDBLinksByApplication(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("app config: list db links: %w", err)
	}
	serverName := map[int64]string{}
	server := func(id int64) (string, error) {
		if n, ok := serverName[id]; ok {
			return n, nil
		}
		inst, err := s.q.GetDBInstance(ctx, id)
		if err != nil {
			return "", fmt.Errorf("app config: get server: %w", err)
		}
		serverName[id] = inst.Name
		return inst.Name, nil
	}
	for _, l := range links {
		cl := ConfigLink{Var: l.VarName, Field: l.Field}
		switch {
		case l.LogicalDatabaseID != nil:
			ld, err := s.q.GetLogicalDatabase(ctx, *l.LogicalDatabaseID)
			if err != nil {
				return nil, fmt.Errorf("app config: get logical database: %w", err)
			}
			cl.Kind, cl.Target = "database", ld.Name
			if cl.Server, err = server(ld.InstanceID); err != nil {
				return nil, err
			}
		case l.InstanceID != nil:
			cl.Kind = "server"
			if cl.Target, err = server(*l.InstanceID); err != nil {
				return nil, err
			}
		default:
			continue
		}
		out = append(out, cl)
	}
	blinks, err := s.q.ListBucketLinksByApplication(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("app config: list bucket links: %w", err)
	}
	for _, l := range blinks {
		b, err := s.q.GetS3Bucket(ctx, l.BucketID)
		if err != nil {
			return nil, fmt.Errorf("app config: get bucket: %w", err)
		}
		cl := ConfigLink{Var: l.VarName, Kind: "bucket", Target: b.Name, Field: l.Field}
		if cl.Server, err = server(b.InstanceID); err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, nil
}
