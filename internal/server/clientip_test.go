package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Behind a reverse proxy every request arrives from the proxy's address, so the
// per-IP login limiter degenerated into ONE bucket for all users: ten failed
// attempts by anybody locked out everybody. Honouring X-Forwarded-For fixes
// that — but only when the operator has said a proxy is in front, since
// otherwise any client could forge the header and get its own private bucket.
func TestClientIPProxyHandling(t *testing.T) {
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// Default: the header is untrusted input and must be ignored entirely.
	if got := clientIP(req("10.0.0.5:5555", "1.2.3.4"), false); got != "10.0.0.5" {
		t.Errorf("untrusted mode used the header: got %q, want %q", got, "10.0.0.5")
	}

	// Trusted mode: a single proxy appends the real client.
	if got := clientIP(req("10.0.0.5:5555", "1.2.3.4"), true); got != "1.2.3.4" {
		t.Errorf("trusted mode: got %q, want %q", got, "1.2.3.4")
	}

	// A client-forged header is followed by the entry the proxy appended, so the
	// LAST entry is the only one the proxy vouches for.
	if got := clientIP(req("10.0.0.5:5555", "9.9.9.9, 5.6.7.8"), true); got != "5.6.7.8" {
		t.Errorf("trusted mode with a forged prefix: got %q, want %q", got, "5.6.7.8")
	}

	// No header, or a malformed one, falls back to the connection address.
	if got := clientIP(req("10.0.0.5:5555", ""), true); got != "10.0.0.5" {
		t.Errorf("missing header: got %q, want %q", got, "10.0.0.5")
	}
	if got := clientIP(req("10.0.0.5:5555", "not-an-ip"), true); got != "10.0.0.5" {
		t.Errorf("malformed header: got %q, want %q", got, "10.0.0.5")
	}
}
