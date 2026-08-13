package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
)

// issueAPIToken mints a genuinely valid, non-expired token for (userID, orgID)
// and persists it, mirroring the `issue` helper in
// internal/api/identity_integration_test.go. It lives here, not imported from
// there, because that helper is in package api_test and this file is in
// package server_test — duplicating a few lines is simpler and more honest
// than reaching across a test-package boundary.
func issueAPIToken(t *testing.T, q *db.Queries, userID, orgID int64, level api.Level) string {
	t.Helper()
	plain, prefix, hash, err := api.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if _, err := q.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		UserID: userID, OrgID: orgID, Name: "test",
		TokenHash: hash, Prefix: prefix, Level: string(level), ExpiresAt: pgtype.Timestamptz{},
	}); err != nil {
		t.Fatalf("create api token: %v", err)
	}
	return plain
}

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

// TestAPIRejectsTokenInQueryString proves the query string is never even
// consulted, not just that a garbage token fails: it mints a token that would
// authenticate perfectly well via the Authorization header, places it ONLY in
// the query string, and asserts the request still 401s. Without a genuinely
// valid token here, this test would pass even after a future ?token= fallback
// was added (as long as that fallback validated tokens) — indistinguishable
// from TestAPIRejectsMissingToken.
func TestAPIRejectsTokenInQueryString(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	uid := mkUser(t, q, "qs-token@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), uid, "QSTokenOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token := issueAPIToken(t, q, uid, o.ID, api.LevelRead)

	rec := httptest.NewRecorder()
	// Even a structurally valid, genuinely-issued token must be ignored here:
	// query strings end up in proxy logs and browser history.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/whoami?token="+token, nil))
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

// TestAPIRateLimitReturns429WithJSONContentType exhausts the per-token limit
// with a genuinely valid token and checks the 429 response itself: both the
// status and that the body is actually labelled application/json. http.Error
// (used by an earlier version of this code) forces
// "Content-Type: text/plain; charset=utf-8" regardless of the body it is
// given, which would silently mislabel this JSON error for any client that
// dispatches on Content-Type.
func TestAPIRateLimitReturns429WithJSONContentType(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	uid := mkUser(t, q, "ratelimited@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), uid, "RateLimitOrg")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token := issueAPIToken(t, q, uid, o.ID, api.LevelRead)

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// apiTokenRateLimit (internal/server/api_middleware.go) currently allows 60
	// requests per token per minute. All calls below land in the same window —
	// the loop runs in milliseconds, nowhere near apiTokenRateWindow — so this
	// is deterministic without sleeping for a real window.
	const limit = 60
	for i := 0; i < limit; i++ {
		if rec := call(); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d/%d was rate-limited before reaching the configured limit", i+1, limit)
		}
	}

	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 after exceeding the per-token limit, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("want Content-Type application/json on the 429 body, got %q", got)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("want a Retry-After header on the 429")
	}
}

// brokenTokenStore is an api.TokenStore whose lookup always fails, standing in
// for "Postgres is unreachable". The error deliberately resembles a real pgx
// dial failure and matches none of api's sentinels, which is the entire point.
type brokenTokenStore struct{}

func (brokenTokenStore) ListAPITokensByPrefix(context.Context, string) ([]db.ApiToken, error) {
	return nil, errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
}

func (brokenTokenStore) TouchAPIToken(context.Context, int64) error { return nil }

// TestAPIDatabaseFailureIsNot401 pins the distinction the whole-branch review
// found missing: a token that could not be JUDGED (the lookup itself failed)
// must not be reported as a token that was judged and rejected. Answering 401
// to an outage tells an agent its credential is bad — it stops retrying and
// escalates — and sends the operator off reissuing perfectly good tokens
// chasing a phantom. Same lesson as commit fc05816.
//
// Revert RequireAPIToken to funnelling every Authenticate error into
// apiUnauthorized and this test fails on the very first assertion.
func TestAPIDatabaseFailureIsNot401(t *testing.T) {
	h, _, _, _ := newDeployServerWithTokens(t, brokenTokenStore{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer krill_pat_anything-at-all")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when the token lookup fails, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	// No challenge: nothing is wrong with the caller's credential, so inviting
	// a retry with a different one is precisely the wrong hint.
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("503 must not carry a WWW-Authenticate challenge, got %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want application/json, got %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (body=%q)", err, rec.Body.String())
	}
	if body["code"] != "unavailable" {
		t.Fatalf("want code=unavailable, got %q", body["code"])
	}
	// The underlying error can carry a DSN; it belongs in the log, not the body.
	if strings.Contains(rec.Body.String(), "5432") {
		t.Fatalf("503 body leaked the internal error: %q", rec.Body.String())
	}
}

// TestAPIBadTokenStillReturns401 is the other half of the same branch: with
// the store healthy, a genuinely unknown token must still be a 401 with a
// challenge. Without it, "everything returns 503" would pass the test above.
func TestAPIBadTokenStillReturns401(t *testing.T) {
	h, _, _, _ := newDeployServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer krill_pat_definitely-not-real")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for an unknown token, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("want a WWW-Authenticate challenge on the 401")
	}
}
