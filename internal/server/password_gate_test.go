package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRequirePasswordChangeFailsClosed proves that a lookup error in
// requirePasswordChange holds the user on the change-password form instead of
// letting the request through. Letting it through on a transient failure would
// reopen the exact hole this middleware exists to close: the inviter still
// knows a flagged account's password until the flag is genuinely cleared.
func TestRequirePasswordChangeFailsClosed(t *testing.T) {
	boom := errors.New("boom")
	s := &Server{
		mustChangePasswordLookup: func(context.Context, int64) (bool, error) {
			return false, boom
		},
	}

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/orgs", nil)
	rec := httptest.NewRecorder()
	s.requirePasswordChange(next).ServeHTTP(rec, req)

	if called {
		t.Fatal("a lookup error must not let the request through")
	}
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account/password" {
		t.Fatalf("want 303 to /account/password, got %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

// TestRequirePasswordChangeLetsThroughOnFalse is the mirror case: a successful
// lookup that reports no pending change lets the request proceed.
func TestRequirePasswordChangeLetsThroughOnFalse(t *testing.T) {
	s := &Server{
		mustChangePasswordLookup: func(context.Context, int64) (bool, error) {
			return false, nil
		},
	}

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/orgs", nil)
	rec := httptest.NewRecorder()
	s.requirePasswordChange(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("a clean 'false' lookup must let the request through")
	}
	if rec.Code != 0 && rec.Code != http.StatusOK {
		t.Fatalf("unexpected redirect: %d", rec.Code)
	}
}

// TestRequirePasswordChangeExemptsTheFormItself proves the form route is never
// gated, even when the lookup would error or report "must change" — otherwise a
// fail-closed lookup failure could redirect-loop the user forever.
func TestRequirePasswordChangeExemptsTheFormItself(t *testing.T) {
	calls := 0
	s := &Server{
		mustChangePasswordLookup: func(context.Context, int64) (bool, error) {
			calls++
			return true, nil
		},
	}

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/account/password", nil)
	rec := httptest.NewRecorder()
	s.requirePasswordChange(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("/account/password must stay reachable")
	}
	if calls != 0 {
		t.Fatalf("the form route must not even consult the lookup, got %d calls", calls)
	}
}
