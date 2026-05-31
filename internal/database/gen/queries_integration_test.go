package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestApplicationCRUD(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		Name:   "web",
		Image:  "nginx",
		Tag:    "alpine",
		Domain: "web.127-0-0-1.sslip.io",
		Port:   80,
		Env:    map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if app.ID == 0 || app.Status != "idle" || app.Env["FOO"] != "bar" {
		t.Fatalf("unexpected app: %+v", app)
	}

	if err := q.UpdateApplicationStatus(ctx, db.UpdateApplicationStatusParams{ID: app.ID, Status: "running"}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, err := q.GetApplication(ctx, app.ID)
	if err != nil || got.Status != "running" {
		t.Fatalf("get after update: %+v err=%v", got, err)
	}

	list, err := q.ListApplications(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: len=%d err=%v", len(list), err)
	}
}

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
