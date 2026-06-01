package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// loginAs создаёт юзера, сессию и возвращает cookie.
func loginAs(t *testing.T, q *db.Queries, email string) *http.Cookie {
	t.Helper()
	authSvc := auth.NewService(q)
	tok, err := authSvc.Authenticate(context.Background(), email, "pw")
	if err != nil {
		t.Fatalf("auth %s: %v", email, err)
	}
	return &http.Cookie{Name: auth.CookieName, Value: tok}
}

func mkUser(t *testing.T, q *db.Queries, email string) int64 {
	t.Helper()
	hash, _ := auth.HashPassword("pw")
	u, err := q.CreateUser(context.Background(), db.CreateUserParams{Email: email, PasswordHash: hash})
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

func newServer(t *testing.T) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	return server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub).Router(), q, orgSvc
}

func TestNonMemberGets404(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Owner Org")
	mkUser(t, q, "outsider@k.local")

	req := httptest.NewRequest(http.MethodGet, "/orgs/"+strconv.FormatInt(o.ID, 10), nil)
	req.AddCookie(loginAs(t, q, "outsider@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("outsider want 404, got %d", rec.Code)
	}
}

func TestMemberCannotCreateProject(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memberID := mkUser(t, q, "member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	form := url.Values{"name": {"P"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAs(t, q, "member@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member create project want 403, got %d", rec.Code)
	}
}

func TestAdminCanCreateProject(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")

	form := url.Values{"name": {"My Project"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAs(t, q, "owner@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("owner create project want 303, got %d", rec.Code)
	}
	ps, _ := q.ListProjects(ctx, o.ID)
	if len(ps) != 1 {
		t.Fatalf("expected 1 project, got %d", len(ps))
	}
}
