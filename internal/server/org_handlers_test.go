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

// loginAs creates a user and a session, and returns a cookie.
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
	dbSvc := dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net")
	return server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbSvc).Router(), q, orgSvc
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

func TestOwnerChangesMemberRole(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memberID := mkUser(t, q, "member@k.local")
	mem, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"})
	if err != nil {
		t.Fatalf("add member: %v", err)
	}

	form := url.Values{"role": {"admin"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members/"+strconv.FormatInt(mem.ID, 10)+"/role", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAs(t, q, "owner@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("owner change role want 303, got %d", rec.Code)
	}
	got, err := q.GetMemberByID(ctx, mem.ID)
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if got.Role != "admin" {
		t.Fatalf("want role admin, got %q", got.Role)
	}
}

func TestCannotDemoteLastOwner(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	ownerMem, err := q.GetMembership(ctx, db.GetMembershipParams{OrganizationID: o.ID, UserID: ownerID})
	if err != nil {
		t.Fatalf("get owner membership: %v", err)
	}

	form := url.Values{"role": {"member"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members/"+strconv.FormatInt(ownerMem.ID, 10)+"/role", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAs(t, q, "owner@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("demote last owner want 403, got %d", rec.Code)
	}
	got, _ := q.GetMemberByID(ctx, ownerMem.ID)
	if got.Role != "owner" {
		t.Fatalf("want role owner unchanged, got %q", got.Role)
	}
}

func TestCannotRemoveLastOwner(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	ownerMem, err := q.GetMembership(ctx, db.GetMembershipParams{OrganizationID: o.ID, UserID: ownerID})
	if err != nil {
		t.Fatalf("get owner membership: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members/"+strconv.FormatInt(ownerMem.ID, 10)+"/remove", nil)
	req.AddCookie(loginAs(t, q, "owner@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remove last owner want 403, got %d", rec.Code)
	}
	if _, err := q.GetMemberByID(ctx, ownerMem.ID); err != nil {
		t.Fatalf("owner should still exist: %v", err)
	}
}

func TestCanRemoveNonLastOwner(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	owner2ID := mkUser(t, q, "owner2@k.local")
	owner2Mem, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: owner2ID, Role: "owner"})
	if err != nil {
		t.Fatalf("add second owner: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members/"+strconv.FormatInt(owner2Mem.ID, 10)+"/remove", nil)
	req.AddCookie(loginAs(t, q, "owner@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("remove non-last owner want 303, got %d", rec.Code)
	}
	if _, err := q.GetMemberByID(ctx, owner2Mem.ID); err == nil {
		t.Fatalf("second owner should be removed")
	}
	n, err := q.CountOwners(ctx, o.ID)
	if err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 owner remaining, got %d", n)
	}
}

func TestMemberCannotChangeRole(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	ownerMem, err := q.GetMembership(ctx, db.GetMembershipParams{OrganizationID: o.ID, UserID: ownerID})
	if err != nil {
		t.Fatalf("get owner membership: %v", err)
	}
	memberID := mkUser(t, q, "member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	form := url.Values{"role": {"admin"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members/"+strconv.FormatInt(ownerMem.ID, 10)+"/role", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAs(t, q, "member@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member change role want 403, got %d", rec.Code)
	}
}

func TestAdminCannotGrantOwner(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	adminID := mkUser(t, q, "admin@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: adminID, Role: "admin"}); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	memberID := mkUser(t, q, "member@k.local")
	mem, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"})
	if err != nil {
		t.Fatalf("add member: %v", err)
	}

	// An admin (not owner) tries to promote the member to owner.
	form := url.Values{"role": {"owner"}}
	req := httptest.NewRequest(http.MethodPost, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/members/"+strconv.FormatInt(mem.ID, 10)+"/role", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(loginAs(t, q, "admin@k.local"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin granting owner want 403, got %d", rec.Code)
	}
	got, _ := q.GetMemberByID(ctx, mem.ID)
	if got.Role != "member" {
		t.Fatalf("want role member unchanged, got %q", got.Role)
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

func TestCreateMemberPasswordNotInURL(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	ownerCookie := loginAs(t, q, "owner@k.local")
	orgPath := "/orgs/" + strconv.FormatInt(o.ID, 10)

	// POST createMember with a brand-new email to trigger temp password generation.
	form := url.Values{"email": {"newbie@k.local"}, "role": {"member"}}
	req := httptest.NewRequest(http.MethodPost, orgPath+"/members", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ownerCookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Must redirect with 303.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("createMember want 303, got %d", rec.Code)
	}

	// Location must be exactly /orgs/{id}/members — no ?temp= in the URL.
	loc := rec.Header().Get("Location")
	wantLoc := orgPath + "/members"
	if loc != wantLoc {
		t.Fatalf("Location want %q, got %q", wantLoc, loc)
	}
	if strings.Contains(loc, "?temp=") {
		t.Fatalf("Location must not contain ?temp=, got %q", loc)
	}

	// A Set-Cookie for krill_flash_pw must be present with a non-empty value and HttpOnly.
	var flashCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "krill_flash_pw" {
			flashCookie = c
			break
		}
	}
	if flashCookie == nil {
		t.Fatal("Set-Cookie krill_flash_pw not found in POST response")
	}
	if flashCookie.Value == "" {
		t.Fatal("krill_flash_pw cookie value is empty")
	}
	if !flashCookie.HttpOnly {
		t.Fatal("krill_flash_pw cookie must be HttpOnly")
	}
	pwValue := flashCookie.Value

	// GET /orgs/{id}/members carrying the flash cookie.
	req2 := httptest.NewRequest(http.MethodGet, orgPath+"/members", nil)
	req2.AddCookie(ownerCookie)
	req2.AddCookie(&http.Cookie{Name: "krill_flash_pw", Value: pwValue})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("listMembers want 200, got %d", rec2.Code)
	}

	// Response body must contain the temp password value.
	body := rec2.Body.String()
	if !strings.Contains(body, pwValue) {
		t.Fatalf("listMembers body does not contain temp password %q", pwValue)
	}

	// The GET response must clear krill_flash_pw (MaxAge <= 0 means expired/deleted).
	var clearCookie *http.Cookie
	for _, c := range rec2.Result().Cookies() {
		if c.Name == "krill_flash_pw" {
			clearCookie = c
			break
		}
	}
	if clearCookie == nil {
		t.Fatal("GET /members must set krill_flash_pw to clear it, but no Set-Cookie found")
	}
	if clearCookie.MaxAge > 0 {
		t.Fatalf("krill_flash_pw clear cookie must have MaxAge <= 0, got %d", clearCookie.MaxAge)
	}
}
