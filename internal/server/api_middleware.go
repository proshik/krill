package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/proshik/krill/internal/api"
)

// apiTokenRate bounds one token's request rate. An agent stuck in a poll loop
// is not a human with a mouse: it can issue hundreds of calls a minute and
// hammer dockerd through the log endpoints.
const (
	apiTokenRateLimit  = 60
	apiTokenRateWindow = time.Minute
)

// tokenLimiter is a fixed-window per-token limiter, the same shape as
// loginRateLimiter but keyed on a token id instead of a source IP.
type tokenLimiter struct {
	mu     sync.Mutex
	hits   map[int64]*rlWindow
	last   time.Time
	limit  int
	window time.Duration
}

func newTokenLimiter(limit int, window time.Duration) *tokenLimiter {
	return &tokenLimiter{hits: map[int64]*rlWindow{}, limit: limit, window: window}
}

// allow records a hit for tokenID and reports whether it is within the limit.
// It opportunistically prunes expired windows at most once per window, the
// same way loginRateLimiter does, so the map cannot grow without bound as
// distinct tokens are used over the process lifetime.
func (l *tokenLimiter) allow(tokenID int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) >= l.window {
		for k, w := range l.hits {
			if now.Sub(w.start) >= l.window {
				delete(l.hits, k)
			}
		}
		l.last = now
	}
	w := l.hits[tokenID]
	if w == nil || now.Sub(w.start) >= l.window {
		l.hits[tokenID] = &rlWindow{count: 1, start: now}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// RequireAPIToken authenticates a bearer token and stashes the resolved
// identity. The token is accepted ONLY from the Authorization header: query
// strings are logged verbatim by reverse proxies and kept in browser history.
func (s *Server) RequireAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiAuth == nil {
			http.NotFound(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			apiUnauthorized(w)
			return
		}
		ident, err := s.apiAuth.Authenticate(r.Context(), strings.TrimPrefix(h, "Bearer "), time.Now())
		if err != nil {
			switch {
			case errors.Is(err, api.ErrInvalidToken),
				errors.Is(err, api.ErrTokenExpired),
				errors.Is(err, api.ErrNoMembership):
				// Deliberately one response for unknown, expired and revoked-membership
				// tokens: telling them apart is free reconnaissance. The distinction
				// goes to the log, never to the client.
				logFrom(r).Info("api token rejected", "err", err)
				apiUnauthorized(w)
			default:
				// The credential was never judged — the lookup itself failed
				// (Postgres down, context cancelled). Answering 401 here would
				// tell a perfectly valid agent its token is bad and send the
				// operator off reissuing tokens to chase an outage.
				logFrom(r).Error("api token authentication failed", "err", err)
				apiUnavailable(w, r)
			}
			return
		}
		if !s.apiLimiter.allow(ident.TokenID, time.Now()) {
			apiRateLimited(w)
			return
		}
		ctx := api.WithIdentity(r.Context(), ident)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func apiUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="krill"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code": "unauthorized", "message": "valid bearer token required",
	})
}

// apiUnavailable writes the 503 for an authentication attempt that could not
// be decided at all because the infrastructure behind it failed. It carries no
// WWW-Authenticate challenge — nothing is wrong with the caller's credential,
// and inviting a retry with a different one is exactly the wrong hint. The
// message stays generic (err.Error() can carry a DSN) but the request id lets
// an operator find the logged cause, the same bargain writeAPIError strikes
// for a 500.
func apiUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code": "unavailable", "message": "authentication is temporarily unavailable, retry shortly",
		"request_id": middleware.GetReqID(r.Context()),
	})
}

// apiRateLimited writes the 429 response for a token that has exceeded
// apiTokenRateLimit. Like apiUnauthorized, it sets Content-Type explicitly:
// http.Error (used by the brief's original code sample) forces
// "text/plain; charset=utf-8" unconditionally, which would mislabel this JSON
// body and break a client that dispatches on Content-Type.
func apiRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(apiTokenRateWindow.Seconds())))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code": "rate_limited", "message": "too many requests for this token",
	})
}

// writeAPIError maps a service-layer error onto an HTTP status and a JSON body.
// Internal errors never leak err.Error(): it can carry a DSN or a host path.
func writeAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var aerr *api.Error
	code, status, msg := "internal", http.StatusInternalServerError, "internal error"
	if errors.As(err, &aerr) {
		code, msg = string(aerr.Code), aerr.Message
		switch aerr.Code {
		case api.CodeNotFound:
			status = http.StatusNotFound
		case api.CodeForbidden:
			status = http.StatusForbidden
		case api.CodeInvalid:
			status = http.StatusBadRequest
		case api.CodeConflict:
			status = http.StatusConflict
		default:
			status = http.StatusInternalServerError
		}
	}
	if status == http.StatusInternalServerError {
		logFrom(r).Error("api internal error", "err", err)
		msg = "internal error, see server logs"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code": code, "message": msg, "request_id": middleware.GetReqID(r.Context()),
	})
}
