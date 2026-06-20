package server

import (
	"net/http"
	"net/url"
	"strings"
)

// csrfGuard rejects state-changing requests whose Origin/Referer is cross-origin.
// Browsers always send Origin on cross-site POSTs, so this stops CSRF; requests
// with NO Origin/Referer (curl, tests, server-to-server) are allowed. It
// complements the SameSite=Lax session cookie as a stateless second layer.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/webhooks/") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = r.Header.Get("Referer")
		}
		if origin != "" {
			if u, err := url.Parse(origin); err != nil || (u.Host != "" && u.Host != r.Host) {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
