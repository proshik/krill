package auth_test

import (
	"context"
	"errors"
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

// Any GetUserByEmail failure collapsed into ErrInvalidCredentials, so a
// database outage told every user "invalid email or password" and left no
// signal that the infrastructure -- not the password -- was the problem. Only a
// genuinely missing row means invalid credentials.
func TestAuthenticateDistinguishesInfraFailureFromBadCredentials(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	svc := auth.NewService(q)

	hash, _ := auth.HashPassword("pw")
	if _, err := q.CreateUser(context.Background(), db.CreateUserParams{Email: "infra@k.local", PasswordHash: hash}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// A genuinely unknown email is still ErrInvalidCredentials.
	if _, err := svc.Authenticate(context.Background(), "nobody@k.local", "pw"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("unknown email: got %v, want ErrInvalidCredentials", err)
	}

	// A failing query (cancelled context stands in for any infra failure) must
	// not be reported as bad credentials.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Authenticate(ctx, "infra@k.local", "pw")
	if err == nil {
		t.Fatal("expected an error when the user lookup fails")
	}
	if errors.Is(err, auth.ErrInvalidCredentials) {
		t.Error("infrastructure failure reported as invalid credentials: the user is told their password is wrong while the database is down")
	}
}

// SeedAdmin promoted the configured admin to instance operator but never
// demoted anyone, so rotating KRILL_ADMIN_EMAIL left the OLD account with
// instance-wide rights over cluster nodes and host monitoring — an operator who
// believed they had handed the role over.
func TestSeedAdminRevokesPreviousOperator(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	svc := auth.NewService(q)
	ctx := context.Background()

	if err := svc.SeedAdmin(ctx, "first@k.local", "pw"); err != nil {
		t.Fatalf("seed first: %v", err)
	}
	first, err := q.GetUserByEmail(ctx, "first@k.local")
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if ok, _ := q.GetUserIsAdmin(ctx, first.ID); !ok {
		t.Fatal("seeded admin was not promoted")
	}

	// Operator rotates KRILL_ADMIN_EMAIL to a different account.
	if err := svc.SeedAdmin(ctx, "second@k.local", "pw"); err != nil {
		t.Fatalf("seed second: %v", err)
	}
	second, err := q.GetUserByEmail(ctx, "second@k.local")
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if ok, _ := q.GetUserIsAdmin(ctx, second.ID); !ok {
		t.Error("new admin was not promoted")
	}
	if ok, _ := q.GetUserIsAdmin(ctx, first.ID); ok {
		t.Error("previous admin kept instance-operator rights after the email was rotated")
	}
}
