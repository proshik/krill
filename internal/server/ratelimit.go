package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// loginRateLimiter is a small fixed-window per-IP limiter for the login
// endpoint. bcrypt already makes each attempt expensive; this caps the attempt
// rate so an attacker cannot run an unbounded online password-guessing attack.
// In-memory and intentionally simple — a self-hosted control plane serves a
// handful of clients. Behind a reverse proxy the connection address is the
// proxy's, which would collapse every client into one bucket, so the limiter
// keys on clientIP: it honours X-Forwarded-For when (and only when) the
// operator has declared a proxy via KRILL_TRUST_PROXY.
type loginRateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*rlWindow
	last   time.Time // last opportunistic prune
}

type rlWindow struct {
	count int
	start time.Time
}

func newLoginRateLimiter(limit int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{limit: limit, window: window, hits: map[string]*rlWindow{}}
}

// allow records an attempt from ip and reports whether it is within the limit.
func (l *loginRateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	w := l.hits[ip]
	if w == nil || now.Sub(w.start) >= l.window {
		l.hits[ip] = &rlWindow{count: 1, start: now}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// pruneLocked drops expired windows at most once per window so the map cannot
// grow without bound under many distinct source IPs.
func (l *loginRateLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.last) < l.window {
		return
	}
	l.last = now
	for ip, w := range l.hits {
		if now.Sub(w.start) >= l.window {
			delete(l.hits, ip)
		}
	}
}

func (l *loginRateLimiter) middleware(trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow(clientIP(r, trustProxy), time.Now()) {
				w.Header().Set("Retry-After", strconv.Itoa(int(l.window.Seconds())))
				http.Error(w, "too many login attempts, try again later", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP returns the request's source IP without the port.
//
// With trustProxy set it prefers the LAST X-Forwarded-For entry: a client may
// forge the header, but the proxy in front appends the address it actually saw,
// so the final entry is the only one anything vouches for. Without trustProxy
// the header is ignored outright — otherwise any client could mint its own
// rate-limit bucket by sending one.
func clientIP(r *http.Request, trustProxy bool) string {
	direct := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		direct = host
	}
	if !trustProxy {
		return direct
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return direct
	}
	parts := strings.Split(xff, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(last) == nil {
		return direct
	}
	return last
}
