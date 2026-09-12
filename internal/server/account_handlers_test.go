package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
)

// loginWithPassword performs a real POST /login (unlike loginAs, which mints a
// session directly without checking a password) so a test can exercise a
// password chosen through the change-password form.
func loginWithPassword(t *testing.T, h http.Handler, email, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"email": {email}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login %s: want 303, got %d body %s", email, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatalf("login %s: no session cookie set", email)
	return nil
}

// TestMustChangePasswordGatesEveryPage exercises the invitee flow end-to-end:
// a user flagged with must_change_password is redirected off every page to
// /account/password (which itself stays reachable), and once they submit a
// new password the flag clears and they pass through normally.
//
// The brief's helper "login" (POST /login returning a bare cookie string) does
// not exist in this package; loginWithPassword above does the same real
// POST /login but returns *http.Cookie, matching the existing postForm helper
// and http.Request.AddCookie used throughout this package's tests.
func TestMustChangePasswordGatesEveryPage(t *testing.T) {
	h, q, _, _ := newDeployServer(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword("temp-password-1")
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "invitee@k.local", PasswordHash: hash})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := q.SetUserMustChangePassword(ctx, db.SetUserMustChangePasswordParams{ID: u.ID, MustChangePassword: true}); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	cookie := loginWithPassword(t, h, "invitee@k.local", "temp-password-1")

	req := httptest.NewRequest(http.MethodGet, "/orgs", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account/password" {
		t.Fatalf("flagged user must be held on the form: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// The form itself must stay reachable, or the user is locked out.
	req = httptest.NewRequest(http.MethodGet, "/account/password", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("password form: want 200, got %d", rec.Code)
	}

	rec = postForm(t, h, "/account/password", cookie, url.Values{
		"current": {"temp-password-1"}, "next": {"chosen-password-1"}, "repeat": {"chosen-password-1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("change: want 303, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/orgs", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("after the change the user must pass: %d", rec.Code)
	}
}

// TestChangePasswordIsReachableFromTheLayout pins the entry point: every
// organization page links to the change-password form — for a plain member
// too, who has no Settings — and that form renders inside the normal layout, so
// the user can navigate away. A user who is not forced to change their password
// and lands on the standalone /account/password is sent to that page as well.
func TestChangePasswordIsReachableFromTheLayout(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()

	ownerID := mkUser(t, q, "owner-pw@k.local")
	o, err := orgSvc.CreateOrg(ctx, ownerID, "Org")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	memberID := mkUser(t, q, "member-pw@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie := loginWithPassword(t, h, "member-pw@k.local", "pw")
	orgURL := "/orgs/" + i64(o.ID)
	pageURL := orgURL + "/account/password"

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get(orgURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="`+pageURL+`"`) {
		t.Fatal("the layout must link to the change-password page")
	}

	rec = get(pageURL)
	if rec.Code != http.StatusOK {
		t.Fatalf("change-password page: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="`+pageURL+`"`) {
		t.Fatal("the in-layout form must post back to the organization page")
	}
	if !strings.Contains(body, `href="`+orgURL+`"`) || !strings.Contains(body, `action="/logout"`) {
		t.Fatal("the change-password page must render inside the normal layout, with navigation")
	}

	rec = get("/account/password")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != pageURL {
		t.Fatalf("a user who is not forced must be sent to the in-layout page, got %d %s", rec.Code, rec.Header().Get("Location"))
	}

	rec = postForm(t, h, pageURL, cookie, url.Values{"current": {"pw"}, "next": {"a-new-password-1"}, "repeat": {"nope"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `action="/logout"`) {
		t.Fatalf("a refused change must re-render inside the layout, got %d", rec.Code)
	}

	rec = postForm(t, h, pageURL, cookie, url.Values{"current": {"pw"}, "next": {"a-new-password-1"}, "repeat": {"a-new-password-1"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != orgURL {
		t.Fatalf("change: want 303 to %s, got %d %s", orgURL, rec.Code, rec.Header().Get("Location"))
	}
	loginWithPassword(t, h, "member-pw@k.local", "a-new-password-1")
}
