package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
)

func TestCanWriteRequiresBothLevelAndRole(t *testing.T) {
	cases := []struct {
		name  string
		level Level
		role  auth.Role
		want  bool
	}{
		{"write token, admin", LevelWrite, auth.RoleAdmin, true},
		{"write token, owner", LevelWrite, auth.RoleOwner, true},
		{"write token, demoted to member", LevelWrite, auth.RoleMember, false},
		{"read token, owner", LevelRead, auth.RoleOwner, false},
		{"read token, member", LevelRead, auth.RoleMember, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := Identity{Level: tc.level, Role: tc.role}
			if got := id.CanWrite(); got != tc.want {
				t.Fatalf("CanWrite() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeTokenStore is a minimal in-memory TokenStore, letting Authenticate be
// unit-tested without a database. It ignores the prefix argument and simply
// returns whatever rows the test seeded, mirroring the fact that the real
// lookup prefix is not unique — the caller must scan every returned row.
type fakeTokenStore struct {
	rows       []db.ApiToken
	touchErr   error
	touchedIDs []int64
}

func (f *fakeTokenStore) ListAPITokensByPrefix(ctx context.Context, prefix string) ([]db.ApiToken, error) {
	return f.rows, nil
}

func (f *fakeTokenStore) TouchAPIToken(ctx context.Context, id int64) error {
	f.touchedIDs = append(f.touchedIDs, id)
	return f.touchErr
}

// fakeMemberResolver is a fixed-answer MemberResolver for unit tests. err is
// set by the test that checks a failed membership lookup is reported as an
// infrastructure failure rather than as "not a member".
type fakeMemberResolver struct {
	role auth.Role
	ok   bool
	err  error
}

func (f *fakeMemberResolver) Membership(ctx context.Context, userID, orgID int64) (auth.Role, bool, error) {
	return f.role, f.ok, f.err
}

// TestAuthenticateSkipsPrefixCollisionToFindRealMatch verifies the loop in
// Authenticate does not stop at the first row sharing a lookup prefix: it
// must keep scanning past a non-matching hash to find the row that actually
// matches. Stopping early would make any token unlucky enough to collide on
// its 8-character prefix permanently unauthenticatable.
func TestAuthenticateSkipsPrefixCollisionToFindRealMatch(t *testing.T) {
	plain, prefix, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// A second, unrelated token whose hash happens to be tried first but does
	// not match — the collision case the lookup prefix does not rule out.
	_, _, otherHash, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	rows := []db.ApiToken{
		{ID: 1, UserID: 100, OrgID: 10, TokenHash: otherHash, Prefix: prefix, Level: string(LevelRead)},
		{ID: 2, UserID: 200, OrgID: 20, TokenHash: hash, Prefix: prefix, Level: string(LevelWrite)},
	}
	store := &fakeTokenStore{rows: rows}
	members := &fakeMemberResolver{role: auth.RoleOwner, ok: true}
	a := NewAuthenticator(store, members)

	id, err := a.Authenticate(context.Background(), plain, time.Now())
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.TokenID != 2 || id.UserID != 200 || id.OrgID != 20 {
		t.Fatalf("resolved the wrong row past the collision: %+v", id)
	}
}

// TestAuthenticateTouchFailureDoesNotFailRequest verifies TouchAPIToken is
// best-effort: a failure to record last_used_at must not turn a successful
// authentication into an error.
func TestAuthenticateTouchFailureDoesNotFailRequest(t *testing.T) {
	plain, prefix, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rows := []db.ApiToken{
		{ID: 7, UserID: 1, OrgID: 2, TokenHash: hash, Prefix: prefix, Level: string(LevelWrite)},
	}
	store := &fakeTokenStore{rows: rows, touchErr: errors.New("touch failed")}
	members := &fakeMemberResolver{role: auth.RoleAdmin, ok: true}
	a := NewAuthenticator(store, members)

	id, err := a.Authenticate(context.Background(), plain, time.Now())
	if err != nil {
		t.Fatalf("authenticate should succeed despite a touch failure: %v", err)
	}
	if id.TokenID != 7 {
		t.Fatalf("wrong identity: %+v", id)
	}
	if len(store.touchedIDs) != 1 || store.touchedIDs[0] != 7 {
		t.Fatalf("touch was not attempted as expected: %+v", store.touchedIDs)
	}
}

// TestAuthenticateReportsMembershipLookupFailure pins the distinction the
// middleware relies on: a token whose owner cannot be checked because the
// query failed must not come back as ErrNoMembership, which the caller maps
// to 401 "valid bearer token required". An agent reading that would conclude
// its credential is bad and a human would go reissue tokens, chasing a
// permissions problem that does not exist.
func TestAuthenticateReportsMembershipLookupFailure(t *testing.T) {
	plain, prefix, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	store := &fakeTokenStore{rows: []db.ApiToken{{ID: 1, UserID: 7, OrgID: 3, TokenHash: hash, Prefix: prefix, Level: string(LevelWrite)}}}
	boom := errors.New("connection refused")
	a := NewAuthenticator(store, &fakeMemberResolver{err: boom})

	_, err = a.Authenticate(context.Background(), plain, time.Now())
	if err == nil {
		t.Fatal("a failed membership lookup was reported as success")
	}
	if errors.Is(err, ErrNoMembership) || errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrTokenExpired) {
		t.Fatalf("infrastructure failure surfaced as a credential sentinel: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("want the underlying error, got %v", err)
	}
}
