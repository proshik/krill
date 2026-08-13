package api

import (
	"context"
	"errors"
	"time"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
)

// Level is the capability stamped on a token at issuance.
type Level string

const (
	LevelRead  Level = "read"
	LevelWrite Level = "write"
)

var (
	ErrInvalidToken = errors.New("invalid api token")
	ErrTokenExpired = errors.New("api token expired")
	ErrNoMembership = errors.New("token owner is no longer a member of the organization")
)

// Identity is the resolved caller behind an API token.
type Identity struct {
	UserID  int64
	OrgID   int64
	TokenID int64
	Level   Level
	Role    auth.Role
}

// CanWrite reports whether this caller may mutate. Both halves must hold: the
// token was issued at write level AND its owner still has at least admin in the
// org. Demoting or removing the member disables their write tokens immediately,
// with no separate revocation step.
func (i Identity) CanWrite() bool {
	return i.Level == LevelWrite && i.Role.AtLeast(auth.RoleAdmin)
}

// TokenStore is the subset of the generated queries the authenticator needs.
type TokenStore interface {
	ListAPITokensByPrefix(ctx context.Context, prefix string) ([]db.ApiToken, error)
	TouchAPIToken(ctx context.Context, id int64) error
}

// MemberResolver reports a user's live role in an organization.
type MemberResolver interface {
	Membership(ctx context.Context, userID, orgID int64) (auth.Role, bool)
}

// Authenticator turns a bearer token into an Identity.
type Authenticator struct {
	tokens  TokenStore
	members MemberResolver
}

func NewAuthenticator(t TokenStore, m MemberResolver) *Authenticator {
	return &Authenticator{tokens: t, members: m}
}

// Authenticate verifies the token and resolves the caller's live rights.
// Every failure returns one of the sentinel errors and no Identity — callers
// must not distinguish "unknown token" from "expired" to the client.
func (a *Authenticator) Authenticate(ctx context.Context, plain string, now time.Time) (Identity, error) {
	if plain == "" {
		return Identity{}, ErrInvalidToken
	}
	rows, err := a.tokens.ListAPITokensByPrefix(ctx, PrefixOf(plain))
	if err != nil {
		return Identity{}, err
	}
	for _, row := range rows {
		if !TokenMatches(row.TokenHash, plain) {
			continue
		}
		if row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(now) {
			return Identity{}, ErrTokenExpired
		}
		role, ok := a.members.Membership(ctx, row.UserID, row.OrgID)
		if !ok {
			return Identity{}, ErrNoMembership
		}
		// Best-effort: a failed touch must not fail the request.
		_ = a.tokens.TouchAPIToken(ctx, row.ID)
		return Identity{
			UserID:  row.UserID,
			OrgID:   row.OrgID,
			TokenID: row.ID,
			Level:   Level(row.Level),
			Role:    role,
		}, nil
	}
	return Identity{}, ErrInvalidToken
}

// identityCtxKey is the context key both agent-facing adapters use to carry
// the caller's resolved Identity from RequireAPIToken (internal/server)
// through to their own handlers. It lives here, in internal/api, rather than
// in internal/server where RequireAPIToken is defined: the MCP adapter
// (internal/mcpsrv) needs to read it too, and internal/mcpsrv must not import
// internal/server (internal/server mounts internal/mcpsrv's handler, so that
// import would cycle). Both adapters importing internal/api already is what
// makes this the one place both can reach.
type identityCtxKey struct{}

// WithIdentity returns a copy of ctx carrying id, retrievable via IdentityFrom.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFrom reads the Identity stashed by WithIdentity. ok is false when
// none is present — a route reachable without going through RequireAPIToken
// first, or (for the MCP adapter) a tool handler invoked on a request that
// somehow bypassed it. Callers should treat that as an internal error, not
// silently zero-value the caller's identity.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	v, ok := ctx.Value(identityCtxKey{}).(Identity)
	return v, ok
}
