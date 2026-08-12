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
