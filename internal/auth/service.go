package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// dummyHash is a valid bcrypt hash compared against on the unknown-email login
// path so that path costs the same as a real bcrypt verification. Without it,
// an unknown email returns in microseconds while a known one spends tens of ms
// in bcrypt — a measurable oracle for enumerating registered accounts.
var dummyHash, _ = HashPassword("krill-constant-time-login-placeholder")

// Authenticate verifies email+password and on success creates a session, returning a token.
func (s *Service) Authenticate(ctx context.Context, email, password string) (string, error) {
	u, err := s.q.GetUserByEmail(ctx, email)
	if err != nil {
		// Only a genuinely missing row is "invalid credentials". Collapsing
		// every failure into it tells users their password is wrong while the
		// database is down, and hides the outage from the operator.
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("look up user: %w", err)
		}
		// Spend the same bcrypt time as a real check so known and unknown emails
		// are indistinguishable by response latency.
		CheckPassword(dummyHash, password)
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

var (
	// ErrWrongPassword is returned when the caller's current password does not
	// match the stored hash.
	ErrWrongPassword = errors.New("current password does not match")
	// ErrWeakPassword is returned when the chosen new password is shorter than
	// MinPasswordLen.
	ErrWeakPassword = errors.New("password is too short")
)

// MinPasswordLen is the floor for a user-chosen password.
const MinPasswordLen = 10

// ChangePassword verifies the current password, stores the new one and revokes
// every other session of that user; keepToken (the caller's own session) stays
// valid so the user is not logged out of the page they are standing on.
func (s *Service) ChangePassword(ctx context.Context, userID int64, current, next, keepToken string) error {
	u, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !CheckPassword(u.PasswordHash, current) {
		return ErrWrongPassword
	}
	if len([]rune(next)) < MinPasswordLen {
		return ErrWeakPassword
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.q.SetUserPassword(ctx, db.SetUserPasswordParams{ID: userID, PasswordHash: hash}); err != nil {
		return err
	}
	return s.q.DeleteSessionsByUserExcept(ctx, db.DeleteSessionsByUserExceptParams{UserID: userID, Token: keepToken})
}

// IsInstanceAdmin reports whether the user is an instance-level operator.
func (s *Service) IsInstanceAdmin(ctx context.Context, userID int64) (bool, error) {
	return s.q.GetUserIsAdmin(ctx, userID)
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

	// Promote the configured admin to instance operator (idempotent). Global
	// infrastructure is gated on this flag, not on org-scoped RoleAdmin.
	if err := s.q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: u.ID, IsAdmin: true}); err != nil {
		return err
	}
	// ...and revoke it from anyone else. The promotion used to be one-way, so
	// rotating KRILL_ADMIN_EMAIL left the previous account with instance-wide
	// rights over cluster nodes and host monitoring.
	if n, err := s.q.DemoteInstanceAdminsExcept(ctx, u.ID); err != nil {
		return err
	} else if n > 0 {
		slog.Warn("revoked instance-operator rights from previously seeded admins", "count", n, "current_admin", email)
	}

	// Default organization: bootstrap it only on a genuinely fresh instance.
	// Checking just this user's memberships meant that rotating the admin email
	// tried to create a SECOND "default" org, whose slug collides -- and the
	// error is fatal at startup, so the rotation bricked the control plane.
	orgs, err := s.q.ListOrganizationsForUser(ctx, u.ID)
	if err != nil {
		return err
	}
	if len(orgs) > 0 {
		return nil
	}
	if n, err := s.q.CountOrganizations(ctx); err != nil {
		return err
	} else if n > 0 {
		slog.Info("skipping default-organization bootstrap: this instance already has organizations", "admin", email)
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
