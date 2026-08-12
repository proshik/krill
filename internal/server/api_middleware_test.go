package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIRejectsMissingToken(t *testing.T) {
	h, _, _, _ := newDeployServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("want a WWW-Authenticate challenge")
	}
}

func TestAPIRejectsTokenInQueryString(t *testing.T) {
	h, _, _, _ := newDeployServer(t)
	rec := httptest.NewRecorder()
	// Even a structurally valid token must be ignored here: query strings end up
	// in proxy logs and browser history.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/whoami?token=krill_pat_whatever", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for query-string token, got %d", rec.Code)
	}
}

func TestAPIRejectsGarbageBearer(t *testing.T) {
	h, _, _, _ := newDeployServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer krill_pat_definitely-not-real")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}
