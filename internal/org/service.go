package org

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
)

// ErrNotFound — resource not found or does not belong to the context.
var ErrNotFound = errors.New("not found")

// ErrSlugTaken — slug already taken within the parent.
var ErrSlugTaken = errors.New("slug already taken")

// Service — business logic for organizations and nested resources on top of sqlc.
type Service struct {
	q *db.Queries
}

func NewService(q *db.Queries) *Service { return &Service{q: q} }

// Membership returns the user's role in the organization (ok=false if not a member).
func (s *Service) Membership(ctx context.Context, userID, orgID int64) (auth.Role, bool) {
	m, err := s.q.GetMembership(ctx, db.GetMembershipParams{OrganizationID: orgID, UserID: userID})
	if err != nil {
		return auth.RoleMember, false
	}
	r, ok := auth.ParseRole(m.Role)
	return r, ok
}

// CreateOrg creates an organization and makes the creator the owner.
func (s *Service) CreateOrg(ctx context.Context, ownerID int64, name string) (db.Organization, error) {
	slug := Slugify(name)
	if slug == "" {
		return db.Organization{}, ErrSlugTaken
	}
	if _, err := s.q.GetOrganizationBySlug(ctx, slug); err == nil {
		return db.Organization{}, ErrSlugTaken
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Organization{}, err
	}
	o, err := s.q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: name, Slug: slug, OwnerID: ownerID})
	if err != nil {
		return db.Organization{}, err
	}
	_, err = s.q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: ownerID, Role: "owner"})
	if err != nil {
		return db.Organization{}, err
	}
	return o, nil
}

// CreateProject creates a project in the organization.
func (s *Service) CreateProject(ctx context.Context, orgID int64, name, description string) (db.Project, error) {
	slug := Slugify(name)
	if slug == "" {
		return db.Project{}, ErrSlugTaken
	}
	p, err := s.q.CreateProject(ctx, db.CreateProjectParams{
		OrganizationID: orgID, Name: name, Slug: slug, Description: description,
	})
	if err != nil {
		return db.Project{}, mapUniqueErr(err)
	}
	return p, nil
}

// CreateEnvironment creates an environment in the project.
func (s *Service) CreateEnvironment(ctx context.Context, projID int64, name string) (db.Environment, error) {
	slug := Slugify(name)
	if slug == "" {
		return db.Environment{}, ErrSlugTaken
	}
	e, err := s.q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: projID, Name: name, Slug: slug})
	if err != nil {
		return db.Environment{}, mapUniqueErr(err)
	}
	return e, nil
}

// ProjectInOrg returns the project only if it belongs to orgID.
func (s *Service) ProjectInOrg(ctx context.Context, orgID, projID int64) (db.Project, error) {
	p, err := s.q.GetProject(ctx, projID)
	if err != nil || p.OrganizationID != orgID {
		return db.Project{}, ErrNotFound
	}
	return p, nil
}

// EnvironmentInProject returns the environment only if it belongs to projID.
func (s *Service) EnvironmentInProject(ctx context.Context, projID, envID int64) (db.Environment, error) {
	e, err := s.q.GetEnvironment(ctx, envID)
	if err != nil || e.ProjectID != projID {
		return db.Environment{}, ErrNotFound
	}
	return e, nil
}

// AppInChain verifies that the application belongs to the org→proj→env chain.
func (s *Service) AppInChain(ctx context.Context, orgID, projID, envID, appID int64) (db.Application, error) {
	chain, err := s.q.GetApplicationChain(ctx, appID)
	if err != nil {
		return db.Application{}, ErrNotFound
	}
	if chain.OrgID != orgID || chain.ProjectID != projID || chain.EnvID != envID {
		return db.Application{}, ErrNotFound
	}
	a, err := s.q.GetApplication(ctx, appID)
	if err != nil {
		return db.Application{}, ErrNotFound
	}
	return a, nil
}

func mapUniqueErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrSlugTaken
	}
	return err
}
