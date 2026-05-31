package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeValidator struct{ okToken string }

func (f fakeValidator) Validate(_ context.Context, token string) (int64, bool) {
	if token == f.okToken {
		return 42, true
	}
	return 0, false
}

func TestRequireAuthRedirectsWithoutCookie(t *testing.T) {
	h := RequireAuth(fakeValidator{okToken: "good"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
}

func TestRequireAuthPassesWithValidCookie(t *testing.T) {
	var gotUID int64
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { gotUID = UserID(r.Context()) })
	h := RequireAuth(fakeValidator{okToken: "good"})(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "good"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotUID != 42 {
		t.Fatalf("want uid 42 in context, got %d", gotUID)
	}
}
