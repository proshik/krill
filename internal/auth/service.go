package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
)

// SessionTTL — session lifetime.
const SessionTTL = 7 * 24 * time.Hour

// Service encapsulates authentication operations on top of sqlc queries.
type Service struct {
	q *db.Queries
}

func NewService(q *db.Queries) *Service { return &Service{q: q} }

// Authenticate verifies email+password and on success creates a session, returning a token.
func (s *Service) Authenticate(ctx context.Context, email, password string) (string, error) {
	u, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		return "", ErrInvalidCredentials
	}
	if !CheckPassword(u.PasswordHash, password) {
		return "", ErrInvalidCredentials
	}
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	err = s.q.CreateSession(ctx, db.CreateSessionParams{
		Token:     token,
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(SessionTTL),
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Validate returns userID if the token is valid and has not expired.
func (s *Service) Validate(ctx context.Context, token string) (int64, bool) {
	sess, err := s.q.GetSession(ctx, token)
	if err != nil {
		return 0, false
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.q.DeleteSession(ctx, token)
		return 0, false
	}
	return sess.UserID, true
}

// Logout removes the session.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.q.DeleteSession(ctx, token)
}

// PruneExpiredSessions deletes all expired session rows. Validate only reaps a
// session lazily when its exact token is re-presented — which never happens
// once the browser drops the cookie — so without periodic pruning the table
// grows with every login forever.
func (s *Service) PruneExpiredSessions(ctx context.Context) error {
	return s.q.DeleteExpiredSessions(ctx)
}

// SeedAdmin creates the admin user and the default organization if they do not exist yet.
// Idempotent: a repeated start does not duplicate or overwrite anything.
func (s *Service) SeedAdmin(ctx context.Context, email, password string) error {
	u, err := s.q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		hash, herr := HashPassword(password)
		if herr != nil {
			return herr
		}
		u, err = s.q.CreateUser(ctx, db.CreateUserParams{Email: email, PasswordHash: hash})
	}
	if err != nil {
		return err
	}

	// Default organization: if the admin has no membership anywhere yet, create it.
	orgs, err := s.q.ListOrganizationsForUser(ctx, u.ID)
	if err != nil {
		return err
	}
	if len(orgs) > 0 {
		return nil
	}
	o, err := s.q.CreateOrganization(ctx, db.CreateOrganizationParams{
		Name: "Default", Slug: "default", OwnerID: u.ID,
	})
	if err != nil {
		return err
	}
	_, err = s.q.CreateMember(ctx, db.CreateMemberParams{
		OrganizationID: o.ID, UserID: u.ID, Role: "owner",
	})
	return err
}

// ErrInvalidCredentials — invalid email or password.
var ErrInvalidCredentials = errors.New("invalid credentials")
