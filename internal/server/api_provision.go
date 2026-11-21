package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/traefik"
	"github.com/proshik/krill/internal/web/i18n"
)

// apiProvisioner implements api.Provisioner with the same building blocks the
// web UI's create forms use (host reservation and sharing, database
// provisioning inside its server, routing labels, link-variable checks), so a
// resource created through the agent API is indistinguishable from one
// created in the UI.
//
// Every operation is idempotent on its natural key: a repeat that already
// took effect returns the existing resource, and one that names an existing
// resource with different settings is a conflict. Each holds a process-wide
// lock on its key from the lookup to the end of the write (Krill is one
// process), so two identical requests in flight — an agent retrying after a
// timeout — cannot both miss the lookup, nor can a repeat answer "exists"
// for a database still being provisioned.
type apiProvisioner struct{ s *Server }

var _ api.Provisioner = apiProvisioner{}

// provisionTimeout bounds creating a database inside its server. It runs on a
// context detached from the request: a client giving up must not leave a
// catalog row without its database, or a database without its row.
const provisionTimeout = 2 * time.Minute

func (p apiProvisioner) log(ctx context.Context) *slog.Logger {
	return slog.Default().With("request_id", middleware.GetReqID(ctx))
}

// lockKey serializes one natural key.
func lockKey(kind string, scope int64, name string) string {
	return "api-provision:" + kind + ":" + strconv.FormatInt(scope, 10) + ":" + name
}

func withKey(ctx context.Context, key string, fn func() error) error {
	if err := oplock.Acquire(ctx, key); err != nil {
		return err
	}
	defer oplock.Release(key)
	return fn()
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

// pick resolves a reference that is a name or a numeric id. A reference that
// is one resource's name and another's id is ambiguous and refused: guessing
// would put a database on the wrong server or credentials on the wrong app.
func pick[T any](ref string, items []T, name func(T) string, id func(T) int64) (T, bool, error) {
	var zero T
	var byName, byID *T
	n, idErr := strconv.ParseInt(ref, 10, 64)
	for i := range items {
		if name(items[i]) == ref && byName == nil {
			byName = &items[i]
		}
		if idErr == nil && id(items[i]) == n {
			byID = &items[i]
		}
	}
	switch {
	case byName != nil && byID != nil && id(*byName) != id(*byID):
		return zero, false, api.Invalid(fmt.Sprintf("%q is one resource's name and another's id; rename one in the web UI", ref))
	case byName != nil:
		return *byName, true, nil
	case byID != nil:
		return *byID, true, nil
	}
	return zero, false, nil
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
	inst, ok, err := pick(ref, all, func(i db.DbInstance) string { return i.Name }, func(i db.DbInstance) int64 { return i.ID })
	if err != nil {
		return db.DbInstance{}, err
	}
	if !ok {
		return db.DbInstance{}, api.NotFound(fmt.Sprintf("no database server %q in this organization", ref))
	}
	return inst, nil
}

func (p apiProvisioner) CreateDatabase(ctx context.Context, env api.EnvRef, in api.CreateDatabaseInput) (api.DatabaseResult, error) {
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
	var res api.DatabaseResult
	err = withKey(ctx, lockKey("db", env.EnvID, name), func() error {
		var e error
		res, e = p.createDatabaseLocked(ctx, env, inst, name, dbName, username)
		return e
	})
	return res, err
}

func (p apiProvisioner) createDatabaseLocked(ctx context.Context, env api.EnvRef, inst db.DbInstance, name, dbName, username string) (api.DatabaseResult, error) {
	log := p.log(ctx)
	rows, err := p.s.q.ListLogicalDatabasesByEnvironment(ctx, env.EnvID)
	if err != nil {
		return api.DatabaseResult{}, fmt.Errorf("list databases: %w", err)
	}
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		if r.InstanceID != inst.ID || r.DbName != dbName || r.Username != username {
			return api.DatabaseResult{}, api.Conflict(fmt.Sprintf("a database %q already exists in %s with different settings (server %q, db_name %q, username %q)", name, env.Path(), r.InstanceName, r.DbName, r.Username))
		}
		return api.DatabaseResult{ID: r.ID, Name: r.Name, DBName: r.DbName, Username: r.Username, Server: inst.Name}, nil
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
	// From the row onwards the work must finish (or be undone) whether or not
	// the caller is still waiting.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionTimeout)
	defer cancel()
	ld, err := p.s.q.CreateLogicalDatabase(wctx, db.CreateLogicalDatabaseParams{
		InstanceID: inst.ID, EnvironmentID: env.EnvID, Name: name, DbName: dbName, Username: username, Password: secret.Enc(pw),
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Same db_name on this server, used from another environment (the
			// name itself was checked under the lock).
			return api.DatabaseResult{}, api.Conflict(fmt.Sprintf("database name %q is already used on server %q", dbName, inst.Name))
		}
		return api.DatabaseResult{}, fmt.Errorf("create database row: %w", err)
	}
	if err := p.s.dbsvc.ProvisionLogicalDB(wctx, di, dbservice.LogicalDB{DBName: dbName, Username: username, Password: pw}); err != nil {
		if derr := p.s.q.DeleteLogicalDatabase(wctx, ld.ID); derr != nil {
			log.Error("api create database: rollback row failed", "err", derr, "ldb_id", ld.ID)
		}
		if errors.Is(err, dbservice.ErrObjectsExist) {
			names := fmt.Sprintf("%q", dbName)
			if username != dbName {
				names = fmt.Sprintf("%q or %q", dbName, username)
			}
			return api.DatabaseResult{}, api.Conflict(fmt.Sprintf("server %q already has a database or role named %s that Krill does not manage; it was left untouched", inst.Name, names))
		}
		log.Error("api create database: provisioning failed", "err", err, "instance_id", inst.ID, "db_name", dbName)
		return api.DatabaseResult{}, fmt.Errorf("provision database %s", dbName)
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
	tls := dom.TLS != nil && *dom.TLS // a new app's primary host is its own
	var registryID *int64
	if ref := strings.TrimSpace(in.Registry); ref != "" {
		regs, err := p.s.q.ListRegistriesByOrg(ctx, env.OrgID)
		if err != nil {
			return api.AppResult{}, fmt.Errorf("list registries: %w", err)
		}
		r, ok, err := pick(ref, regs, func(r db.Registry) string { return r.Name }, func(r db.Registry) int64 { return r.ID })
		if err != nil {
			return api.AppResult{}, err
		}
		if !ok {
			return api.AppResult{}, api.NotFound(fmt.Sprintf("no registry %q in this organization", ref))
		}
		registryID = &r.ID
	}
	path := env.Path() + "/" + name

	var res api.AppResult
	err = withKey(ctx, lockKey("app", env.EnvID, name), func() error {
		apps, err := p.s.q.ListApplicationsByEnvironment(ctx, env.EnvID)
		if err != nil {
			return fmt.Errorf("list apps: %w", err)
		}
		for _, a := range apps {
			if a.Name != name {
				continue
			}
			same := a.SourceType == "image" && a.Image == image && a.Tag == tag && int(a.Port) == in.Port && ptrEq(a.RegistryID, registryID)
			if same {
				doms, derr := p.s.q.ListDomainsByApplication(ctx, a.ID)
				if derr != nil {
					return fmt.Errorf("list domains: %w", derr)
				}
				same = slices.ContainsFunc(doms, func(d db.Domain) bool {
					return d.Host == host && d.IsPrimary && sameDomain(d, tls, dom.Exposed, paths)
				})
			}
			if !same {
				return api.Conflict(fmt.Sprintf("an app %q already exists in %s with different settings; change it in the web UI, or deploy it with a new tag", name, env.Path()))
			}
			res = api.AppResult{ID: a.ID, Path: path, Domain: a.Domain}
			return nil
		}
		if p.s.hosts == nil {
			return fmt.Errorf("host-sharing store unavailable")
		}
		a, err := p.s.hosts.CreateApplicationConfigured(ctx, db.CreateApplicationParams{
			EnvironmentID: env.EnvID, Name: name, Image: image, Tag: tag, Domain: host, Port: int32(in.Port),
			SourceType: "image", GitBranch: "main", DockerfilePath: "Dockerfile",
		}, hostshare.PrimarySettings{Exposed: dom.Exposed, Paths: paths, TLS: tls, RegistryID: registryID})
		if err != nil {
			if c := hostConflict(ctx, err); c != nil {
				return c
			}
			if isUniqueViolation(err) {
				return api.Conflict(fmt.Sprintf("host %q is already used by another app", host))
			}
			return fmt.Errorf("create app: %w", err)
		}
		res = api.AppResult{ID: a.ID, Path: path, Domain: host, Created: true}
		return nil
	})
	return res, err
}

func (p apiProvisioner) AddDomain(ctx context.Context, app db.Application, in api.DomainInput) (api.DomainResult, error) {
	log := p.log(ctx)
	host, paths, err := p.domainSpec(ctx, in)
	if err != nil {
		return api.DomainResult{}, err
	}
	var res api.DomainResult
	err = withKey(ctx, lockKey("domain", app.ID, host), func() error {
		// A host shared by several apps has one TLS setting: an omitted value
		// takes it, an explicit one must agree (changing it is a UI action
		// that affects every app on the host).
		claims, err := p.s.q.ListDomainClaimsByHost(ctx, host)
		if err != nil {
			return fmt.Errorf("list host claims: %w", err)
		}
		tls := in.TLS != nil && *in.TLS
		if len(claims) > 0 {
			hostTLS := claims[0].Tls
			if in.TLS != nil && *in.TLS != hostTLS {
				return api.Conflict(fmt.Sprintf("host %q is served with HTTPS %s for every app on it; omit tls or change it in the web UI", host, onOffWord(hostTLS)))
			}
			tls = hostTLS
		}
		doms, err := p.s.q.ListDomainsByApplication(ctx, app.ID)
		if err != nil {
			return fmt.Errorf("list domains: %w", err)
		}
		for _, d := range doms {
			if d.Host != host {
				continue
			}
			if !sameDomain(d, tls, in.Exposed, paths) {
				return api.Conflict(fmt.Sprintf("the app already has %q with different settings; change it in the web UI", host))
			}
			// A repeat re-applies the routing labels (a no-op when they are
			// already right): the first attempt may have stored the domain and
			// then failed to update them.
			if !p.s.syncAppLabelsCtx(ctx, log, app.ID, app.Port) {
				return api.Unavailable("the domain exists, but the gateway's routing could not be updated; retry")
			}
			res = domainResult(d, false)
			return nil
		}
		if p.s.hosts == nil {
			return fmt.Errorf("host-sharing store unavailable")
		}
		d, err := p.s.hosts.Add(ctx, hostshare.AddParams{AppID: app.ID, Host: host, TLS: tls, Exposed: in.Exposed, Paths: paths})
		if err != nil {
			if c := hostConflict(ctx, err); c != nil {
				return c
			}
			return fmt.Errorf("add domain: %w", err)
		}
		if !p.s.syncAppLabelsCtx(ctx, log, app.ID, app.Port) {
			return api.Unavailable("the domain was added, but the gateway's routing could not be updated; retry the same request")
		}
		res = domainResult(d, true)
		return nil
	})
	return res, err
}

func onOffWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
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
	var res api.LinkResult
	link := func() error {
		var ldbID, instID *int64
		var drv drivers.Driver
		var target, scheme string
		if in.Database != "" {
			scheme = strings.TrimSpace(in.Scheme)
			if scheme == "" {
				scheme = "postgres"
			}
			if scheme != "postgres" && scheme != "postgresql" {
				return api.Invalid("scheme must be postgres or postgresql")
			}
			rows, err := p.s.q.ListLogicalDatabasesByEnvironment(ctx, app.EnvironmentID)
			if err != nil {
				return fmt.Errorf("list databases: %w", err)
			}
			for _, r := range rows {
				if r.Name == in.Database {
					id := r.ID
					ldbID = &id
					target = r.Name + " @ " + r.InstanceName
				}
			}
			if ldbID == nil {
				return api.NotFound(fmt.Sprintf("no database %q in the app's environment", in.Database))
			}
			drv, _ = drivers.Registry.Get("postgres")
		} else {
			inst, err := p.findServer(ctx, chain.OrgID, in.Server)
			if err != nil {
				return err
			}
			if inst.Engine == "postgres" {
				return api.Invalid(fmt.Sprintf("%q is a Postgres server: link one of its logical databases with database", inst.Name))
			}
			d, ok := drivers.Registry.Get(inst.Engine)
			if !ok {
				return api.Invalid(fmt.Sprintf("database server %q has an engine this Krill does not know", inst.Name))
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
			return api.Invalid(fmt.Sprintf("field %q does not apply here; one of: %s", field, strings.Join(drv.LinkFields(), ", ")))
		}
		links, err := p.s.q.ListDBLinksByApplication(ctx, app.ID)
		if err != nil {
			return fmt.Errorf("list links: %w", err)
		}
		for _, l := range links {
			if l.VarName != varName {
				continue
			}
			if !(ptrEq(l.LogicalDatabaseID, ldbID) && ptrEq(l.InstanceID, instID) && l.Field == field && l.Scheme == scheme) {
				return api.Conflict(fmt.Sprintf("%s is already linked to another database or field", varName))
			}
			res = api.LinkResult{ID: l.ID, Var: varName, Target: target, Field: field}
			return nil
		}
		if key := p.s.linkVarTakenCtx(ctx, p.log(ctx), app, varName); key != "" {
			return api.Conflict(english(ctx, key, varName))
		}
		l, err := p.s.q.CreateDBLink(ctx, db.CreateDBLinkParams{
			ApplicationID: app.ID, LogicalDatabaseID: ldbID, InstanceID: instID, VarName: varName, Scheme: scheme, Field: field,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return api.Conflict(fmt.Sprintf("%s is already linked", varName))
			}
			return fmt.Errorf("create link: %w", err)
		}
		res = api.LinkResult{ID: l.ID, Var: varName, Target: target, Field: field, Created: true}
		return nil
	}
	err = withKey(ctx, lockKey("link", app.ID, varName), func() error {
		if in.Database == "" {
			return link()
		}
		// The database may be one an identical create is still provisioning:
		// wait for it, so a link never points at a row that may yet roll back.
		return withKey(ctx, lockKey("db", app.EnvironmentID, in.Database), link)
	})
	return res, err
}

func ptrEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
