package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/traefik"
	"github.com/proshik/krill/internal/web/i18n"
)

// apiProvisioner implements api.Provisioner with the same building blocks the
// web UI's create forms use (host reservation and sharing rules, database
// provisioning inside its server, routing labels, link-variable checks), so
// a resource created through the agent API is indistinguishable from one
// created in the UI. Every operation is idempotent on its natural key: a
// repeat that already took effect returns the existing resource, and one that
// names an existing resource with different settings is a conflict.
type apiProvisioner struct{ s *Server }

var _ api.Provisioner = apiProvisioner{}

func (p apiProvisioner) log(ctx context.Context) *slog.Logger {
	return slog.Default().With("request_id", middleware.GetReqID(ctx))
}

// english renders an i18n message key for an API error: the agent API speaks
// English whatever the operator's UI language.
func english(ctx context.Context, key string, args ...any) string {
	return i18n.Tf(i18n.WithLocale(ctx, "en"), key, args...)
}

// hostConflict maps a host-sharing refusal onto an API conflict.
func hostConflict(ctx context.Context, err error) error {
	var c *hostshare.ConflictError
	if errors.As(err, &c) {
		return api.Conflict(english(ctx, c.Key, c.Args...))
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgerr *pgconn.PgError
	return errors.As(err, &pgerr) && pgerr.Code == "23505"
}

// findServer resolves a database server of the organization by name or id.
func (p apiProvisioner) findServer(ctx context.Context, orgID int64, ref string) (db.DbInstance, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return db.DbInstance{}, api.Invalid("server is required: a database server's name or id")
	}
	all, err := p.s.q.ListDBInstancesByOrg(ctx, orgID)
	if err != nil {
		return db.DbInstance{}, fmt.Errorf("list database servers: %w", err)
	}
	id, idErr := strconv.ParseInt(ref, 10, 64)
	for _, inst := range all {
		if inst.Name == ref || (idErr == nil && inst.ID == id) {
			return inst, nil
		}
	}
	return db.DbInstance{}, api.NotFound(fmt.Sprintf("no database server %q in this organization", ref))
}

func (p apiProvisioner) CreateDatabase(ctx context.Context, env api.EnvRef, in api.CreateDatabaseInput) (api.DatabaseResult, error) {
	log := p.log(ctx)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return api.DatabaseResult{}, api.Invalid("name is required")
	}
	inst, err := p.findServer(ctx, env.OrgID, in.Server)
	if err != nil {
		return api.DatabaseResult{}, err
	}
	if inst.Engine != "postgres" {
		return api.DatabaseResult{}, api.Invalid(fmt.Sprintf("database server %q is %s: logical databases live in a Postgres server; link a %s server to an app directly", inst.Name, inst.Engine, inst.Engine))
	}
	dbName := strings.TrimSpace(in.DBName)
	if dbName == "" {
		dbName = dbservice.SanitizeIdent(name)
	}
	username := strings.TrimSpace(in.Username)
	if username == "" {
		username = dbName
	}
	if !dbservice.ValidIdent(dbName) || !dbservice.ValidIdent(username) {
		return api.DatabaseResult{}, api.Invalid("db_name and username must be lower-case identifiers (letters, digits and _, starting with a letter)")
	}
	existing := func() (api.DatabaseResult, bool, error) {
		rows, err := p.s.q.ListLogicalDatabasesByEnvironment(ctx, env.EnvID)
		if err != nil {
			return api.DatabaseResult{}, false, fmt.Errorf("list databases: %w", err)
		}
		for _, r := range rows {
			if r.Name != name {
				continue
			}
			if r.InstanceID != inst.ID || r.DbName != dbName || r.Username != username {
				return api.DatabaseResult{}, true, api.Conflict(fmt.Sprintf("a database %q already exists in %s with different settings (server %q, db_name %q, username %q)", name, env.Path(), r.InstanceName, r.DbName, r.Username))
			}
			return api.DatabaseResult{ID: r.ID, Name: r.Name, DBName: r.DbName, Username: r.Username, Server: inst.Name}, true, nil
		}
		return api.DatabaseResult{}, false, nil
	}
	if res, found, err := existing(); found || err != nil {
		return res, err
	}
	instPW, err := secret.Dec(inst.SuperuserPassword)
	if err != nil {
		log.Error("api create database: server password undecryptable", "err", err, "instance_id", inst.ID)
		return api.DatabaseResult{}, fmt.Errorf("database server password cannot be decrypted")
	}
	di := dbservice.Instance{AppName: inst.AppName, Superuser: inst.Superuser, SuperuserPassword: instPW}
	if p.s.dbsvc == nil || !p.s.dbsvc.InstanceRunning(ctx, di) {
		return api.DatabaseResult{}, api.Unavailable(fmt.Sprintf("database server %q is not running", inst.Name))
	}
	pw, err := genPassword()
	if err != nil {
		return api.DatabaseResult{}, fmt.Errorf("generate password: %w", err)
	}
	ld, err := p.s.q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: env.EnvID, Name: name, DbName: dbName, Username: username, Password: secret.Enc(pw),
	})
	if err != nil {
		if isUniqueViolation(err) {
			// A concurrent identical request won the race: answer as a repeat.
			if res, found, err := existing(); found || err != nil {
				return res, err
			}
			return api.DatabaseResult{}, api.Conflict(fmt.Sprintf("database name %q is already used on server %q", dbName, inst.Name))
		}
		return api.DatabaseResult{}, fmt.Errorf("create database row: %w", err)
	}
	if err := p.s.dbsvc.ProvisionLogicalDB(ctx, di, dbservice.LogicalDB{DBName: dbName, Username: username, Password: pw}); err != nil {
		if derr := p.s.q.DeleteLogicalDatabase(ctx, ld.ID); derr != nil {
			log.Error("api create database: rollback row failed", "err", derr, "ldb_id", ld.ID)
		}
		log.Error("api create database: provisioning failed", "err", err, "instance_id", inst.ID, "db_name", dbName)
		return api.DatabaseResult{}, fmt.Errorf("provision database: %w", err)
	}
	return api.DatabaseResult{ID: ld.ID, Name: name, DBName: dbName, Username: username, Server: inst.Name, Created: true}, nil
}

// domainSpec validates a requested domain.
func (p apiProvisioner) domainSpec(ctx context.Context, in api.DomainInput) (host string, paths []string, err error) {
	host = strings.ToLower(strings.TrimSpace(in.Host))
	if !validHost(host) {
		return "", nil, api.Invalid(fmt.Sprintf("invalid host %q", in.Host))
	}
	if reserved, rerr := p.s.hostReservedByPanel(ctx, host); rerr != nil {
		return "", nil, fmt.Errorf("check panel host: %w", rerr)
	} else if reserved {
		return "", nil, api.Conflict(fmt.Sprintf("host %q is the Krill panel's own domain", host))
	}
	paths, perr := validPaths(strings.Join(in.Paths, "\n"))
	if perr != nil {
		return "", nil, api.Invalid(perr.Error())
	}
	return host, traefik.NormalizePaths(paths), nil
}

func domainResult(d db.Domain, created bool) api.DomainResult {
	paths := traefik.NormalizePaths(traefik.SplitPaths(d.Paths))
	if paths == nil {
		paths = []string{}
	}
	return api.DomainResult{ID: d.ID, Host: d.Host, TLS: d.Tls, Exposed: d.Exposed, Paths: paths, Primary: d.IsPrimary, Created: created}
}

func sameDomain(d db.Domain, tls, exposed bool, paths []string) bool {
	return d.Tls == tls && d.Exposed == exposed && !d.RedirectToPrimary &&
		slices.Equal(traefik.NormalizePaths(traefik.SplitPaths(d.Paths)), paths)
}

func (p apiProvisioner) CreateApp(ctx context.Context, env api.EnvRef, in api.CreateAppInput) (api.AppResult, error) {
	log := p.log(ctx)
	name := strings.TrimSpace(in.Name)
	if !isSlug(name) {
		return api.AppResult{}, api.Invalid(fmt.Sprintf("invalid app name %q: lower-case letters, digits and dashes", in.Name))
	}
	image := strings.TrimSpace(in.Image)
	if image == "" {
		return api.AppResult{}, api.Invalid("image is required (a repository without a tag, e.g. ghcr.io/acme/bot)")
	}
	tag := strings.TrimSpace(in.Tag)
	if tag == "" {
		tag = "latest"
	}
	if in.Port < 1 || in.Port > 65535 {
		return api.AppResult{}, api.Invalid("port must be 1..65535: the port the container listens on")
	}
	dom := api.DomainInput{Host: name + "." + p.s.cfg.BaseDomain}
	if in.Domain != nil {
		dom = *in.Domain
	}
	host, paths, err := p.domainSpec(ctx, dom)
	if err != nil {
		return api.AppResult{}, err
	}
	var registryID *int64
	if ref := strings.TrimSpace(in.Registry); ref != "" {
		regs, err := p.s.q.ListRegistriesByOrg(ctx, env.OrgID)
		if err != nil {
			return api.AppResult{}, fmt.Errorf("list registries: %w", err)
		}
		n, nErr := strconv.ParseInt(ref, 10, 64)
		for _, r := range regs {
			if r.Name == ref || (nErr == nil && r.ID == n) {
				id := r.ID
				registryID = &id
				break
			}
		}
		if registryID == nil {
			return api.AppResult{}, api.NotFound(fmt.Sprintf("no registry %q in this organization", ref))
		}
	}
	path := env.Path() + "/" + name

	apps, err := p.s.q.ListApplicationsByEnvironment(ctx, env.EnvID)
	if err != nil {
		return api.AppResult{}, fmt.Errorf("list apps: %w", err)
	}
	for _, a := range apps {
		if a.Name != name {
			continue
		}
		same := a.SourceType == "image" && a.Image == image && int(a.Port) == in.Port &&
			((registryID == nil && a.RegistryID == nil) || (registryID != nil && a.RegistryID != nil && *registryID == *a.RegistryID))
		if same {
			doms, derr := p.s.q.ListDomainsByApplication(ctx, a.ID)
			if derr != nil {
				return api.AppResult{}, fmt.Errorf("list domains: %w", derr)
			}
			same = slices.ContainsFunc(doms, func(d db.Domain) bool {
				return d.Host == host && d.IsPrimary && sameDomain(d, dom.TLS, dom.Exposed, paths)
			})
		}
		if !same {
			return api.AppResult{}, api.Conflict(fmt.Sprintf("an app %q already exists in %s with different settings; change it in the web UI, or deploy it with a new tag", name, env.Path()))
		}
		return api.AppResult{ID: a.ID, Path: path, Domain: a.Domain}, nil
	}

	if p.s.hosts == nil {
		return api.AppResult{}, fmt.Errorf("host-sharing store unavailable")
	}
	a, err := p.s.hosts.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: env.EnvID, Name: name, Image: image, Tag: tag, Domain: host, Port: int32(in.Port),
		SourceType: "image", GitBranch: "main", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		if c := hostConflict(ctx, err); c != nil {
			return api.AppResult{}, c
		}
		if isUniqueViolation(err) {
			return api.AppResult{}, api.Conflict(fmt.Sprintf("an app %q or host %q already exists", name, host))
		}
		return api.AppResult{}, fmt.Errorf("create app: %w", err)
	}
	if registryID != nil {
		if err := p.s.q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: a.ID, RegistryID: registryID}); err != nil {
			log.Error("api create app: set registry failed", "err", err, "app_id", a.ID)
			return api.AppResult{}, fmt.Errorf("app %s created, but setting its registry failed: %w", path, err)
		}
	}
	if dom.Exposed || len(paths) > 0 || dom.TLS {
		doms, err := p.s.q.ListDomainsByApplication(ctx, a.ID)
		if err != nil || len(doms) == 0 {
			return api.AppResult{}, fmt.Errorf("app %s created, but its domain could not be read back: %v", path, err)
		}
		primary := doms[0]
		if dom.Exposed || len(paths) > 0 {
			if err := p.s.hosts.UpdatePaths(ctx, primary.ID, dom.Exposed, paths); err != nil {
				if c := hostConflict(ctx, err); c != nil {
					return api.AppResult{}, c
				}
				return api.AppResult{}, fmt.Errorf("app %s created, but exposing its domain failed: %w", path, err)
			}
		}
		if dom.TLS {
			if _, err := p.s.hosts.SetTLS(ctx, primary.ID, true); err != nil {
				return api.AppResult{}, fmt.Errorf("app %s created, but turning on HTTPS failed: %w", path, err)
			}
		}
	}
	return api.AppResult{ID: a.ID, Path: path, Domain: host, Created: true}, nil
}

func (p apiProvisioner) AddDomain(ctx context.Context, app db.Application, in api.DomainInput) (api.DomainResult, error) {
	log := p.log(ctx)
	host, paths, err := p.domainSpec(ctx, in)
	if err != nil {
		return api.DomainResult{}, err
	}
	doms, err := p.s.q.ListDomainsByApplication(ctx, app.ID)
	if err != nil {
		return api.DomainResult{}, fmt.Errorf("list domains: %w", err)
	}
	for _, d := range doms {
		if d.Host != host {
			continue
		}
		if !sameDomain(d, in.TLS, in.Exposed, paths) {
			return api.DomainResult{}, api.Conflict(fmt.Sprintf("the app already has %q with different settings; change it in the web UI", host))
		}
		// A repeat re-applies the routing labels: the first attempt may have
		// stored the domain and then failed to update them.
		if !p.s.syncAppLabelsCtx(ctx, log, app.ID, app.Port) {
			return api.DomainResult{}, api.Unavailable("the domain exists, but the gateway's routing could not be updated; retry")
		}
		return domainResult(d, false), nil
	}
	if p.s.hosts == nil {
		return api.DomainResult{}, fmt.Errorf("host-sharing store unavailable")
	}
	d, err := p.s.hosts.Add(ctx, hostshare.AddParams{AppID: app.ID, Host: host, TLS: in.TLS, Exposed: in.Exposed, Paths: paths})
	if err != nil {
		if c := hostConflict(ctx, err); c != nil {
			return api.DomainResult{}, c
		}
		return api.DomainResult{}, fmt.Errorf("add domain: %w", err)
	}
	if !p.s.syncAppLabelsCtx(ctx, log, app.ID, app.Port) {
		return api.DomainResult{}, api.Unavailable("the domain was added, but the gateway's routing could not be updated; retry the same request")
	}
	return domainResult(d, true), nil
}

func (p apiProvisioner) LinkDatabase(ctx context.Context, app db.Application, in api.LinkDatabaseInput) (api.LinkResult, error) {
	varName := strings.TrimSpace(in.Var)
	if !envVarNameRe.MatchString(varName) {
		return api.LinkResult{}, api.Invalid(fmt.Sprintf("invalid variable name %q", in.Var))
	}
	if (in.Database == "") == (in.Server == "") {
		return api.LinkResult{}, api.Invalid("set exactly one of database (a logical database of the app's environment) and server (a non-Postgres database server)")
	}
	field := strings.TrimSpace(in.Field)
	if field == "" {
		field = "url"
	}
	chain, err := p.s.q.GetApplicationChain(ctx, app.ID)
	if err != nil {
		return api.LinkResult{}, fmt.Errorf("resolve app: %w", err)
	}
	var ldbID, instID *int64
	var drv drivers.Driver
	var target, scheme string
	if in.Database != "" {
		scheme = strings.TrimSpace(in.Scheme)
		if scheme == "" {
			scheme = "postgres"
		}
		if scheme != "postgres" && scheme != "postgresql" {
			return api.LinkResult{}, api.Invalid("scheme must be postgres or postgresql")
		}
		rows, err := p.s.q.ListLogicalDatabasesByEnvironment(ctx, app.EnvironmentID)
		if err != nil {
			return api.LinkResult{}, fmt.Errorf("list databases: %w", err)
		}
		for _, r := range rows {
			if r.Name == in.Database {
				id := r.ID
				ldbID = &id
				target = r.Name + " @ " + r.InstanceName
			}
		}
		if ldbID == nil {
			return api.LinkResult{}, api.NotFound(fmt.Sprintf("no database %q in the app's environment", in.Database))
		}
		drv, _ = drivers.Registry.Get("postgres")
	} else {
		inst, err := p.findServer(ctx, chain.OrgID, in.Server)
		if err != nil {
			return api.LinkResult{}, err
		}
		if inst.Engine == "postgres" {
			return api.LinkResult{}, api.Invalid(fmt.Sprintf("%q is a Postgres server: link one of its logical databases with database", inst.Name))
		}
		d, ok := drivers.Registry.Get(inst.Engine)
		if !ok {
			return api.LinkResult{}, api.Invalid(fmt.Sprintf("database server %q has an engine this Krill does not know", inst.Name))
		}
		drv = d
		switch inst.Engine {
		case "redis", "dragonfly", "valkey":
			scheme = "redis"
		case "minio":
			scheme = ""
		}
		id := inst.ID
		instID = &id
		target = inst.Name
	}
	if !slices.Contains(drv.LinkFields(), field) {
		return api.LinkResult{}, api.Invalid(fmt.Sprintf("field %q does not apply here; one of: %s", field, strings.Join(drv.LinkFields(), ", ")))
	}
	links, err := p.s.q.ListDBLinksByApplication(ctx, app.ID)
	if err != nil {
		return api.LinkResult{}, fmt.Errorf("list links: %w", err)
	}
	for _, l := range links {
		if l.VarName != varName {
			continue
		}
		same := ptrEq(l.LogicalDatabaseID, ldbID) && ptrEq(l.InstanceID, instID) && l.Field == field && l.Scheme == scheme
		if !same {
			return api.LinkResult{}, api.Conflict(fmt.Sprintf("%s is already linked to another database or field", varName))
		}
		return api.LinkResult{ID: l.ID, Var: varName, Target: target, Field: field}, nil
	}
	if key := p.s.linkVarTakenCtx(ctx, p.log(ctx), app, varName); key != "" {
		return api.LinkResult{}, api.Conflict(english(ctx, key, varName))
	}
	l, err := p.s.q.CreateDBLink(ctx, db.CreateDBLinkParams{
		ApplicationID: app.ID, LogicalDatabaseID: ldbID, InstanceID: instID, VarName: varName, Scheme: scheme, Field: field,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return api.LinkResult{}, api.Conflict(fmt.Sprintf("%s is already linked", varName))
		}
		return api.LinkResult{}, fmt.Errorf("create link: %w", err)
	}
	return api.LinkResult{ID: l.ID, Var: varName, Target: target, Field: field, Created: true}, nil
}

func ptrEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
