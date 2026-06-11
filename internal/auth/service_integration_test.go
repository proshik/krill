package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestSeedAuthenticateValidate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	svc := auth.NewService(db.New(pool))
	ctx := context.Background()

	// Seed idempotency.
	if err := svc.SeedAdmin(ctx, "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.SeedAdmin(ctx, "admin@k.local", "different"); err != nil {
		t.Fatalf("seed second: %v", err)
	}

	// Wrong password.
	if _, err := svc.Authenticate(ctx, "admin@k.local", "different"); err == nil {
		t.Error("second seed must NOT overwrite password")
	}
	// Correct password → token.
	token, err := svc.Authenticate(ctx, "admin@k.local", "pw")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	uid, ok := svc.Validate(ctx, token)
	if !ok || uid == 0 {
		t.Fatalf("validate failed: uid=%d ok=%v", uid, ok)
	}
	// Logout invalidates it.
	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, ok := svc.Validate(ctx, token); ok {
		t.Error("token must be invalid after logout")
	}
}

// TestPruneExpiredSessions verifies the session GC: expired rows are deleted,
// live sessions survive.
func TestPruneExpiredSessions(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	svc := auth.NewService(q)
	ctx := context.Background()

	if err := svc.SeedAdmin(ctx, "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	live, err := svc.Authenticate(ctx, "admin@k.local", "pw")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	// An expired session row (the kind Validate never sees again).
	u, _ := q.GetUserByEmail(ctx, "admin@k.local")
	const stale = "stale-token"
	if err := q.CreateSession(ctx, db.CreateSessionParams{Token: stale, UserID: u.ID, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("create stale session: %v", err)
	}

	if err := svc.PruneExpiredSessions(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := q.GetSession(ctx, stale); err == nil {
		t.Error("expired session must be pruned")
	}
	if _, ok := svc.Validate(ctx, live); !ok {
		t.Error("live session must survive pruning")
	}
}

func TestSeedAdminBootstrapsOrg(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	svc := auth.NewService(q)
	ctx := context.Background()

	if err := svc.SeedAdmin(ctx, "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// repeated seed is idempotent
	if err := svc.SeedAdmin(ctx, "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed twice: %v", err)
	}

	u, err := q.GetUserByEmail(ctx, "admin@k.local")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	orgs, err := q.ListOrganizationsForUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Slug != "default" {
		t.Fatalf("expected exactly 1 default org, got %+v", orgs)
	}
}
