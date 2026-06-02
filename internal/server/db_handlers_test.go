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

func TestCreateDatabaseInvalidPort400(t *testing.T) {
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

	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"

	// non-numeric port
	form := url.Values{"engine": {"postgres"}, "name": {"db1"}, "external_port": {"abc"}}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid port 'abc' want 400, got %d", rec.Code)
	}

	// out-of-range port
	form2 := url.Values{"engine": {"postgres"}, "name": {"db2"}, "external_port": {"99999"}}
	req2 := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("invalid port '99999' want 400, got %d", rec2.Code)
	}

	// no row should have been created
	rows, _ := q.ListPostgresByEnvironment(ctx, e.ID)
	if len(rows) != 0 {
		t.Fatalf("expected no postgres rows, got %d", len(rows))
	}
}

func TestCreateDatabaseDuplicatePort400(t *testing.T) {
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

	target := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/databases"

	// first DB with port 55001 — should succeed
	form1 := url.Values{"engine": {"postgres"}, "name": {"first"}, "external_port": {"55001"}}
	req1 := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form1.Encode()))
	req1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req1.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusSeeOther {
		t.Fatalf("first DB want 303, got %d", rec1.Code)
	}

	// second DB (redis) with same port — should fail
	form2 := url.Values{"engine": {"redis"}, "name": {"second"}, "external_port": {"55001"}}
	req2 := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form2.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("duplicate port want 400, got %d", rec2.Code)
	}

	// only the first row should exist
	pgRows, _ := q.ListPostgresByEnvironment(ctx, e.ID)
	rdRows, _ := q.ListRedisByEnvironment(ctx, e.ID)
	if len(pgRows) != 1 {
		t.Fatalf("expected 1 postgres row, got %d", len(pgRows))
	}
	if len(rdRows) != 0 {
		t.Fatalf("expected 0 redis rows, got %d", len(rdRows))
	}
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }
