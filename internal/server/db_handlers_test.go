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
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

func TestMemberCannotCreateDatabase(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	orgSvc := org.NewService(q)
	hashOwner, _ := auth.HashPassword("pw")
	owner, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "o@k", PasswordHash: hashOwner})
	o, _ := orgSvc.CreateOrg(ctx, owner.ID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	hashM, _ := auth.HashPassword("pw")
	mem, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "m@k", PasswordHash: hashM})
	_, _ = q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: mem.ID, Role: "member"})

	authSvc := auth.NewService(q)
	tok, _ := authSvc.Authenticate(ctx, "m@k", "pw")
	h := server.New(config.Config{BaseDomain: "x", Network: "krill-net", Host: "localhost"}, authSvc, orgSvc, q, nil, nil, deploy.NewLogHub(), dbservice.New(nil, dbservice.NewDBStore(q), deploy.NewLogHub(), "krill-net")).Router()

	form := url.Values{"engine": {"postgres"}, "name": {"db"}}
	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member create db want 403, got %d", rec.Code)
	}
}

func TestAdminCreatesPostgres(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()
	orgSvc := org.NewService(q)
	hashOwner, _ := auth.HashPassword("pw")
	owner, _ := q.CreateUser(ctx, db.CreateUserParams{Email: "o@k", PasswordHash: hashOwner})
	o, _ := orgSvc.CreateOrg(ctx, owner.ID, "Org")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "P", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	authSvc := auth.NewService(q)
	tok, _ := authSvc.Authenticate(ctx, "o@k", "pw")
	h := server.New(config.Config{BaseDomain: "x", Network: "krill-net", Host: "localhost"}, authSvc, orgSvc, q, nil, nil, deploy.NewLogHub(), dbservice.New(nil, dbservice.NewDBStore(q), deploy.NewLogHub(), "krill-net")).Router()

	form := url.Values{"engine": {"postgres"}, "name": {"maindb"}, "version": {"postgres:17"}}
	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin create want 303, got %d", rec.Code)
	}
	rows, _ := q.ListPostgresByEnvironment(ctx, e.ID)
	if len(rows) != 1 || rows[0].Name != "maindb" {
		t.Fatalf("expected 1 pg, got %+v", rows)
	}
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }
