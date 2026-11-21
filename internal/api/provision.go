package api

import (
	"context"
	"fmt"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
)

// Provisioning: four operations that CREATE resources, available only to a
// token issued at provision level whose owner is still an organization admin.
// Nothing here deletes or changes an existing resource. Every operation is
// idempotent on a natural key: repeating a request that already took effect
// returns the existing resource with Created=false, and a request naming an
// existing resource with different settings is a conflict — changing a
// resource stays a job for the web UI.
//
// The work itself (validation shared with the web UI, host reservation,
// provisioning the database inside its server, routing labels) is done by a
// Provisioner the server package implements; this layer owns the permission
// gate and the tenancy of the environment and app references.

// EnvRef is a resolved environment of the caller's organization.
type EnvRef struct {
	OrgID       int64
	ProjectID   int64
	EnvID       int64
	ProjectName string
	EnvName     string
}

// Path is the environment's "project/environment" reference.
func (e EnvRef) Path() string { return e.ProjectName + "/" + e.EnvName }

// CreateDatabaseInput creates a logical database inside an existing Postgres
// server of the organization.
type CreateDatabaseInput struct {
	Name     string `json:"name"`
	Server   string `json:"server"`             // database server name or numeric id
	DBName   string `json:"db_name,omitempty"`  // default: derived from name
	Username string `json:"username,omitempty"` // default: db_name
}

// DatabaseResult describes a logical database; the password is never returned.
type DatabaseResult struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	DBName   string `json:"db_name"`
	Username string `json:"username"`
	Server   string `json:"server"`
	Created  bool   `json:"created"`
}

// DomainInput is one domain of an application. TLS is a pointer because a
// host shared by several apps has one TLS setting for all of them: an omitted
// value takes the host's, an explicit one must agree with it.
type DomainInput struct {
	Host    string   `json:"host"`
	TLS     *bool    `json:"tls,omitempty"`
	Exposed bool     `json:"exposed,omitempty"`
	Paths   []string `json:"paths,omitempty"`
}

// CreateAppInput creates an image application. It is not deployed.
type CreateAppInput struct {
	Name     string       `json:"name"`
	Image    string       `json:"image"`
	Tag      string       `json:"tag,omitempty"` // default "latest"
	Port     int          `json:"port"`
	Registry string       `json:"registry,omitempty"` // registry name or numeric id, for a private image
	Domain   *DomainInput `json:"domain,omitempty"`   // primary domain; default <name>.<base domain>, internal
}

// AppResult describes a created (or already existing) application.
type AppResult struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	Domain  string `json:"domain"`
	Created bool   `json:"created"`
}

// DomainResult describes a domain of an application.
type DomainResult struct {
	ID      int64    `json:"id"`
	Host    string   `json:"host"`
	TLS     bool     `json:"tls"`
	Exposed bool     `json:"exposed"`
	Paths   []string `json:"paths"`
	Primary bool     `json:"primary"`
	Created bool     `json:"created"`
}

// LinkDatabaseInput injects a database's connection value into one of the
// application's environment variables on every deploy. Exactly one of
// Database (a logical database of the app's own environment) and Server (a
// non-Postgres database server of the organization) is set.
type LinkDatabaseInput struct {
	Var      string `json:"var"`
	Database string `json:"database,omitempty"`
	Server   string `json:"server,omitempty"`
	Field    string `json:"field,omitempty"`  // default "url" (Postgres, Redis-compatible); S3: endpoint, access_key, secret_key or region
	Scheme   string `json:"scheme,omitempty"` // postgres links: "postgres" (default) or "postgresql"
}

// LinkResult describes a database link.
type LinkResult struct {
	ID      int64  `json:"id"`
	Var     string `json:"var"`
	Target  string `json:"target"`
	Field   string `json:"field"`
	Created bool   `json:"created"`
}

// Provisioner performs the creations. Implementations return *Error for
// anything the caller can act on.
type Provisioner interface {
	CreateDatabase(ctx context.Context, env EnvRef, in CreateDatabaseInput) (DatabaseResult, error)
	CreateApp(ctx context.Context, env EnvRef, in CreateAppInput) (AppResult, error)
	AddDomain(ctx context.Context, app db.Application, in DomainInput) (DomainResult, error)
	LinkDatabase(ctx context.Context, app db.Application, in LinkDatabaseInput) (LinkResult, error)
}

// SetProvisioner wires the provisioning operations; without one they answer
// unavailable.
func (s *Service) SetProvisioner(p Provisioner) { s.prov = p }

// requireProvision is the gate of every creating operation. Like
// requireWrite, it runs before any reference is resolved, so a token without
// the right cannot probe which environments or apps exist.
func requireProvision(id Identity) error {
	if !id.CanProvision() {
		return Forbidden("creating resources needs a provision-level token whose owner is an admin of the organization")
	}
	return nil
}

func (s *Service) provisioner() (Provisioner, error) {
	if s.prov == nil {
		return nil, Unavailable("provisioning is not available on this installation")
	}
	return s.prov, nil
}

// resolveEnv finds a "project/environment" reference in the caller's
// organization. Not found is the answer for anything outside it.
func (s *Service) resolveEnv(ctx context.Context, id Identity, ref string) (EnvRef, error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(ref), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return EnvRef{}, Invalid(fmt.Sprintf("environment %q: want a project/environment path", ref))
	}
	projects, err := s.q.ListProjects(ctx, id.OrgID)
	if err != nil {
		return EnvRef{}, fmt.Errorf("resolve environment: list projects: %w", err)
	}
	var found []EnvRef
	for _, p := range projects {
		if p.Name != parts[0] {
			continue
		}
		envs, err := s.q.ListEnvironments(ctx, p.ID)
		if err != nil {
			return EnvRef{}, fmt.Errorf("resolve environment: list environments: %w", err)
		}
		for _, e := range envs {
			if e.Name == parts[1] {
				found = append(found, EnvRef{OrgID: id.OrgID, ProjectID: p.ID, EnvID: e.ID, ProjectName: p.Name, EnvName: e.Name})
			}
		}
	}
	switch len(found) {
	case 0:
		return EnvRef{}, NotFound(fmt.Sprintf("no environment %q in this organization", ref))
	case 1:
		return found[0], nil
	}
	// Project names are display names; two can be equal. Creating in a guessed
	// one is worse than refusing.
	return EnvRef{}, Invalid(fmt.Sprintf("environment %q is ambiguous: %d projects share that name; rename one in the web UI", ref, len(found)))
}

// CreateDatabase creates a logical database in env on an existing Postgres
// server, or returns the one already there under that name.
func (s *Service) CreateDatabase(ctx context.Context, id Identity, envRef string, in CreateDatabaseInput) (DatabaseResult, error) {
	if err := requireProvision(id); err != nil {
		return DatabaseResult{}, err
	}
	p, err := s.provisioner()
	if err != nil {
		return DatabaseResult{}, err
	}
	env, err := s.resolveEnv(ctx, id, envRef)
	if err != nil {
		return DatabaseResult{}, err
	}
	return p.CreateDatabase(ctx, env, in)
}

// CreateApp creates an image application in env (not deployed), or returns
// the one already there under that name when it matches.
func (s *Service) CreateApp(ctx context.Context, id Identity, envRef string, in CreateAppInput) (AppResult, error) {
	if err := requireProvision(id); err != nil {
		return AppResult{}, err
	}
	p, err := s.provisioner()
	if err != nil {
		return AppResult{}, err
	}
	env, err := s.resolveEnv(ctx, id, envRef)
	if err != nil {
		return AppResult{}, err
	}
	return p.CreateApp(ctx, env, in)
}

// AddDomain adds a domain to an application, or returns it when the app
// already has it with the same settings.
func (s *Service) AddDomain(ctx context.Context, id Identity, ref string, in DomainInput) (DomainResult, error) {
	if err := requireProvision(id); err != nil {
		return DomainResult{}, err
	}
	p, err := s.provisioner()
	if err != nil {
		return DomainResult{}, err
	}
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return DomainResult{}, err
	}
	return p.AddDomain(ctx, app, in)
}

// LinkDatabase links a database to one of an application's variables, or
// returns the link when it already exists with the same target.
func (s *Service) LinkDatabase(ctx context.Context, id Identity, ref string, in LinkDatabaseInput) (LinkResult, error) {
	if err := requireProvision(id); err != nil {
		return LinkResult{}, err
	}
	p, err := s.provisioner()
	if err != nil {
		return LinkResult{}, err
	}
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return LinkResult{}, err
	}
	return p.LinkDatabase(ctx, app, in)
}
