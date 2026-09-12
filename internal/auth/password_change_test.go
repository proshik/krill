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

func TestChangePassword(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	svc := auth.NewService(q)
	ctx := context.Background()

	hash, _ := auth.HashPassword("old-password")
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "pw@k.local", PasswordHash: hash})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	keep, _ := auth.NewToken()
	other, _ := auth.NewToken()
	for _, tok := range []string{keep, other} {
		if err := q.CreateSession(ctx, db.CreateSessionParams{
			Token: tok, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	if err := svc.ChangePassword(ctx, u.ID, "wrong", "new-password-1", keep); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatalf("wrong current password: want ErrWrongPassword, got %v", err)
	}
	if err := svc.ChangePassword(ctx, u.ID, "old-password", "short", keep); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("short new password: want ErrWeakPassword, got %v", err)
	}
	if err := svc.ChangePassword(ctx, u.ID, "old-password", "new-password-1", keep); err != nil {
		t.Fatalf("change: %v", err)
	}
	if _, ok := svc.Validate(ctx, other); ok {
		t.Fatal("other sessions must be revoked")
	}
	if _, ok := svc.Validate(ctx, keep); !ok {
		t.Fatal("the current session must survive")
	}
	if _, err := svc.Authenticate(ctx, "pw@k.local", "new-password-1"); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	must, err := q.GetUserMustChangePassword(ctx, u.ID)
	if err != nil || must {
		t.Fatalf("flag must be cleared: %v, %v", must, err)
	}
}
