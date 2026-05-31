package auth_test

import (
	"context"
	"testing"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestSeedAuthenticateValidate(t *testing.T) {
	pool := testutil.NewTestDB(t)
	svc := auth.NewService(db.New(pool))
	ctx := context.Background()

	// Идемпотентность seed.
	if err := svc.SeedAdmin(ctx, "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.SeedAdmin(ctx, "admin@k.local", "different"); err != nil {
		t.Fatalf("seed second: %v", err)
	}

	// Неверный пароль.
	if _, err := svc.Authenticate(ctx, "admin@k.local", "different"); err == nil {
		t.Error("second seed must NOT overwrite password")
	}
	// Верный пароль → токен.
	token, err := svc.Authenticate(ctx, "admin@k.local", "pw")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	uid, ok := svc.Validate(ctx, token)
	if !ok || uid == 0 {
		t.Fatalf("validate failed: uid=%d ok=%v", uid, ok)
	}
	// Logout инвалидирует.
	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, ok := svc.Validate(ctx, token); ok {
		t.Error("token must be invalid after logout")
	}
}
