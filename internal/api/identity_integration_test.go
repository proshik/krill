package api_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

// seedTokenFixture creates a user, an org with that user as owner, and returns
// the queries handle plus the ids.
func seedTokenFixture(t *testing.T) (*db.Queries, *org.Service, int64, int64) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	authSvc := auth.NewService(q)
	if err := authSvc.SeedAdmin(t.Context(), "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	orgSvc := org.NewService(q)
	u, err := q.GetUserByEmail(t.Context(), "admin@k.local")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	// Note the argument order: CreateOrg(ctx, ownerID, name).
	o, err := orgSvc.CreateOrg(t.Context(), u.ID, "acme")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	return q, orgSvc, u.ID, o.ID
}

func issue(t *testing.T, q *db.Queries, userID, orgID int64, level api.Level, expires pgtype.Timestamptz) string {
	t.Helper()
	plain, prefix, hash, err := api.GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := q.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		UserID: userID, OrgID: orgID, Name: "test",
		TokenHash: hash, Prefix: prefix, Level: string(level), ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return plain
}

func TestAuthenticateResolvesLiveRole(t *testing.T) {
	q, orgSvc, userID, orgID := seedTokenFixture(t)
	plain := issue(t, q, userID, orgID, api.LevelWrite, pgtype.Timestamptz{})

	a := api.NewAuthenticator(q, orgSvc)
	id, err := a.Authenticate(t.Context(), plain, time.Now())
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.OrgID != orgID || id.UserID != userID {
		t.Fatalf("wrong identity: %+v", id)
	}
	if !id.CanWrite() {
		t.Fatal("owner with write token should be able to write")
	}
}

func TestAuthenticateRejectsUnknownAndExpired(t *testing.T) {
	q, orgSvc, userID, orgID := seedTokenFixture(t)
	a := api.NewAuthenticator(q, orgSvc)

	if _, err := a.Authenticate(t.Context(), "krill_pat_nope", time.Now()); err == nil {
		t.Fatal("unknown token accepted")
	}

	past := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
	expired := issue(t, q, userID, orgID, api.LevelWrite, past)
	if _, err := a.Authenticate(t.Context(), expired, time.Now()); err == nil {
		t.Fatal("expired token accepted")
	}
}

// TestAuthenticateFailsAfterMembershipRemoved is the exact scenario the
// design exists for: remove the token owner from the org and the token dies
// immediately, with no separate revocation step. The last-owner guard that
// blocks this in the UI (internal/server/org_handlers.go, CountOwners) lives
// at the HTTP handler layer, not in org.Service or the generated queries, so
// deleting the seeded owner's own membership row directly is safe here.
func TestAuthenticateFailsAfterMembershipRemoved(t *testing.T) {
	q, orgSvc, userID, orgID := seedTokenFixture(t)
	plain := issue(t, q, userID, orgID, api.LevelWrite, pgtype.Timestamptz{})

	m, err := q.GetMembership(t.Context(), db.GetMembershipParams{OrganizationID: orgID, UserID: userID})
	if err != nil {
		t.Fatalf("get membership: %v", err)
	}
	if err := q.DeleteMember(t.Context(), m.ID); err != nil {
		t.Fatalf("delete member: %v", err)
	}

	a := api.NewAuthenticator(q, orgSvc)
	if _, err := a.Authenticate(t.Context(), plain, time.Now()); err == nil {
		t.Fatal("token whose owner is no longer a member was accepted")
	}
}

// TestAuthenticateReResolvesRoleOnEveryCall proves rights are resolved live,
// not cached on the token: the same write token can write, then loses that
// right the instant its owner is demoted below admin, on the very next call
// with no re-issue and no separate revocation step. A future "optimization"
// that trusted the token's stamped level instead of re-checking membership
// would pass every other test in this file but fail this one.
func TestAuthenticateReResolvesRoleOnEveryCall(t *testing.T) {
	q, orgSvc, userID, orgID := seedTokenFixture(t)
	plain := issue(t, q, userID, orgID, api.LevelWrite, pgtype.Timestamptz{})
	a := api.NewAuthenticator(q, orgSvc)

	before, err := a.Authenticate(t.Context(), plain, time.Now())
	if err != nil {
		t.Fatalf("authenticate (before demotion): %v", err)
	}
	if !before.CanWrite() {
		t.Fatal("owner with write token should be able to write before demotion")
	}

	m, err := q.GetMembership(t.Context(), db.GetMembershipParams{OrganizationID: orgID, UserID: userID})
	if err != nil {
		t.Fatalf("get membership: %v", err)
	}
	if err := q.UpdateMemberRole(t.Context(), db.UpdateMemberRoleParams{ID: m.ID, Role: "member"}); err != nil {
		t.Fatalf("demote: %v", err)
	}

	after, err := a.Authenticate(t.Context(), plain, time.Now())
	if err != nil {
		t.Fatalf("authenticate (after demotion): %v", err)
	}
	if after.CanWrite() {
		t.Fatal("write token should stop writing the instant its owner is demoted below admin")
	}
}
