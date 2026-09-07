package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
)

const testToken = "krill_pat_supersecrettokenvalue000000000000000000000"

func newTestClient(t *testing.T, h http.HandlerFunc) (*client.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, testToken, "krill-cli/test")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c, srv
}

func TestNewValidatesServerAndToken(t *testing.T) {
	tests := []struct{ name, server, token string }{
		{"empty server", "", testToken},
		{"no scheme", "krill.example.com", testToken},
		{"unsupported scheme", "ftp://krill.example.com", testToken},
		{"empty token", "https://krill.example.com", ""},
		{"blank token", "https://krill.example.com", "   "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.New(tc.server, tc.token, "ua"); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestRequestCarriesBearerAndUserAgent(t *testing.T) {
	var gotAuth, gotUA, gotAccept, gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		gotAccept, gotQuery = r.Header.Get("Accept"), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"org_name":"acme","can_write":true}`))
	})
	if _, err := c.Whoami(context.Background()); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUA != "krill-cli/test" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q", gotAccept)
	}
	// The token must never travel in the query string: proxies log those.
	if strings.Contains(gotQuery, "token") {
		t.Fatalf("query string carries a token: %q", gotQuery)
	}
}

// TestTokenNeverAppearsInErrors is the same guard internal/notify keeps on
// its bot token. An error string ends up in scrollback, screenshots and
// pasted bug reports, so it must not carry the credential regardless of which
// failure path produced it.
func TestTokenNeverAppearsInErrors(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		// A server that is closed immediately: connecting fails.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		c, err := client.New(url, testToken, "ua")
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		_, err = c.Whoami(context.Background())
		if err == nil {
			t.Fatal("want a transport error")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("error leaks the token: %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"internal","message":"boom","request_id":"r1"}`))
		})
		_, err := c.Whoami(context.Background())
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("error leaks the token or is nil: %v", err)
		}
	})

	t.Run("unreadable body", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not json at all`))
		})
		_, err := c.Whoami(context.Background())
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("error leaks the token or is nil: %v", err)
		}
	})
}

func TestErrorEnvelopeDecoding(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		contentTyp string
		wantCode   string
		wantMsg    string
	}{
		{"invalid", 400, `{"code":"invalid","message":"bad tag","request_id":"r1"}`, "application/json", "invalid", "bad tag"},
		{"unauthorized", 401, `{"code":"unauthorized","message":"valid bearer token required"}`, "application/json", "unauthorized", "valid bearer token required"},
		{"forbidden", 403, `{"code":"forbidden","message":"read-only"}`, "application/json", "forbidden", "read-only"},
		{"not found", 404, `{"code":"not_found","message":"no such app"}`, "application/json", "not_found", "no such app"},
		{"conflict", 409, `{"code":"conflict","message":"already in progress"}`, "application/json", "conflict", "already in progress"},
		{"internal", 500, `{"code":"internal","message":"internal error, see server logs","request_id":"r9"}`, "application/json", "internal", "internal error, see server logs"},

		// The router-level 404s. These are the ones a JSON-only decoder turns
		// into a parse error, hiding both "this server has the agent API
		// switched off" and "this verb does not exist".
		{"plain text 404", 404, "404 page not found\n", "text/plain; charset=utf-8", "http_404", "404 page not found"},
		{"empty body 404", 404, "", "text/plain", "http_404", "Not Found"},
		// A proxy in front of Krill answering with HTML.
		{"html from a proxy", 502, "<html><body>Bad Gateway</body></html>", "text/html", "http_502", "<html><body>Bad Gateway</body></html>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentTyp)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.Whoami(context.Background())
			var ae *client.APIError
			if !errors.As(err, &ae) {
				t.Fatalf("want *APIError, got %T: %v", err, err)
			}
			if ae.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", ae.HTTPStatus, tc.status)
			}
			if ae.Code != tc.wantCode {
				t.Fatalf("Code = %q, want %q", ae.Code, tc.wantCode)
			}
			if ae.Message != tc.wantMsg {
				t.Fatalf("Message = %q, want %q", ae.Message, tc.wantMsg)
			}
		})
	}
}

func TestRateLimitAndRetryAfter(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","message":"too many requests for this token"}`))
	})
	_, err := c.Whoami(context.Background())
	var ae *client.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if !ae.IsRateLimited() {
		t.Fatal("want IsRateLimited")
	}
	if ae.RetryAfter != 60*time.Second {
		t.Fatalf("RetryAfter = %v, want 60s", ae.RetryAfter)
	}
	// The client must not retry on its own — the budget is shared with
	// whatever else uses this token.
	if calls != 1 {
		t.Fatalf("want exactly 1 request, got %d", calls)
	}
}

// TestConflictIsNotRetryable pins the semantics: another deploy is running,
// so the answer is to wait for that one, not to resend. Each accepted deploy
// rewrites the app's tag, so a retry loop is not a free operation.
func TestConflictIsNotRetryable(t *testing.T) {
	conflict := &client.APIError{HTTPStatus: 409, Code: "conflict"}
	if conflict.IsRetryable() {
		t.Fatal("409 must not be classified as retryable")
	}
	for _, s := range []int{429, 503} {
		if !(&client.APIError{HTTPStatus: s}).IsRetryable() {
			t.Fatalf("%d should be retryable", s)
		}
	}
}

// TestAppRefKeepsLiteralSlashes is the routing contract: the server captures
// everything after /apps/ as one wildcard and splits it on real separators,
// so a percent-encoded slash would match no application at all.
func TestAppRefKeepsLiteralSlashes(t *testing.T) {
	var gotPath, gotRawPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawPath = r.URL.Path, r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"deployment_id":7,"status":"running"}`))
	})
	if _, err := c.Deploy(context.Background(), "acme/production/bot", "v1"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	const want = "/api/v1/apps/acme/production/bot/deploy"
	if gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if strings.Contains(gotRawPath, "%2F") || strings.Contains(gotRawPath, "%2f") {
		t.Fatalf("slashes were escaped: %q", gotRawPath)
	}
}

func TestDeployOmitsAnEmptyTag(t *testing.T) {
	var body string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		body = string(b)
		_, _ = w.Write([]byte(`{"deployment_id":7,"status":"running"}`))
	})
	// After an image upload the server has already set the reference; asking
	// it to retag would move the app to a tag that was never uploaded.
	if _, err := c.Deploy(context.Background(), "17", ""); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if strings.Contains(body, "tag") {
		t.Fatalf("empty tag must not be sent at all, body was %q", body)
	}
}

func TestLogsQueryParameters(t *testing.T) {
	var q string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"t":"2026-09-06T10:00:00Z","lvl":"error","msg":"boom"}]`))
	})
	lines, err := c.Logs(context.Background(), "17", 500, "error")
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(q, "tail=500") || !strings.Contains(q, "level=error") {
		t.Fatalf("query = %q", q)
	}
	if len(lines) != 1 || lines[0].Message != "boom" {
		t.Fatalf("unexpected lines: %+v", lines)
	}
}

// TestAppStatusFlattens pins that the server's embedded struct arrives as one
// flat object rather than a nested one.
func TestAppStatusFlattens(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":17,"path":"acme/production/bot","name":"bot",
			"source_type":"image","image":"ghcr.io/acme/bot","tag":"v1.2.3",
			"status":"running","replicas":"1/1","node":"control-plane",
			"last_deploy_id":418,"last_deploy_status":"done"}`))
	})
	st, err := c.AppStatus(context.Background(), "17")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.ID != 17 || st.Image != "ghcr.io/acme/bot" || st.Tag != "v1.2.3" {
		t.Fatalf("summary fields did not flatten: %+v", st)
	}
	if st.Replicas != "1/1" || st.LastDeployID != 418 {
		t.Fatalf("detail fields missing: %+v", st)
	}
}

// TestRedirectsAreRefused pins the failure mode that only shows up after the
// expensive part. Go rewrites a redirected POST into a body-less GET
// (301/302/303), so an operator's http→https redirect in front of Krill turns
// `POST /apps/x/deploy {"tag":…}` into `GET /apps/x/deploy` — which the router
// answers 404 with the tag silently dropped. Every pre-flight check is a GET
// and sails through, so this used to surface only after the build AND the
// push.
func TestRedirectsAreRefused(t *testing.T) {
	var sawMethods []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethods = append(sawMethods, r.Method)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer front.Close()

	c, err := client.New(front.URL, "krill_pat_x", "test")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = c.Deploy(context.Background(), "acme/production/bot", "v2")
	if err == nil {
		t.Fatal("a redirected deploy reported success")
	}
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Code != "redirected" {
		t.Fatalf("want a redirect error, got %v", err)
	}
	if !strings.Contains(err.Error(), target.URL) {
		t.Fatalf("the error should name where the redirect points, got %v", err)
	}
	if len(sawMethods) != 0 {
		t.Fatalf("the request was forwarded as %v; a redirected POST arrives as a body-less GET", sawMethods)
	}
}

// TestRedirectIsCaughtOnTheFirstReadToo: the point of refusing is that the
// very first call fails, long before anything is built.
func TestRedirectIsCaughtOnTheFirstReadToo(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/api/v1/whoami", http.StatusFound)
	}))
	defer front.Close()

	c, err := client.New(front.URL, "krill_pat_x", "test")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := c.Whoami(context.Background()); err == nil {
		t.Fatal("a redirected whoami reported success")
	}
}
