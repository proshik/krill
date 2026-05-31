package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestUserAndSession(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "a@b.c", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	err = q.CreateSession(ctx, db.CreateSessionParams{
		Token:     "tok",
		UserID:    u.ID,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	s, err := q.GetSession(ctx, "tok")
	if err != nil || s.UserID != u.ID {
		t.Fatalf("get session: %+v err=%v", s, err)
	}
}
