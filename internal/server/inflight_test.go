package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInflightPending(t *testing.T) {
	tr := NewInflight()
	inside := make(chan []string, 1)
	h := tr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inside <- tr.Pending()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp?token=secret", nil))
	got := <-inside
	if len(got) != 1 || !strings.HasPrefix(got[0], "GET /mcp (") {
		t.Fatalf("pending while serving: %q", got)
	}
	if strings.Contains(got[0], "secret") {
		t.Fatalf("the query string leaked into the description: %q", got[0])
	}
	if after := tr.Pending(); len(after) != 0 {
		t.Fatalf("a finished request is still pending: %q", after)
	}

	// Shutdown does not wait for a hijacked WebSocket, so it is not listed.
	ws := httptest.NewRequest(http.MethodGet, "/orgs/1/logs", nil)
	ws.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(httptest.NewRecorder(), ws)
	if got := <-inside; len(got) != 0 {
		t.Fatalf("a WebSocket upgrade is listed as pending: %q", got)
	}
}
