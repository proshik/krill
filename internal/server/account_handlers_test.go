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
