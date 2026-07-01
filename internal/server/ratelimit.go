package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// loginRateLimiter is a small fixed-window per-IP limiter for the login
// endpoint. bcrypt already makes each attempt expensive; this caps the attempt
// rate so an attacker cannot run an unbounded online password-guessing attack.
// In-memory and intentionally simple — a self-hosted control plane serves a
// handful of clients. Note: it keys on RemoteAddr, so behind a reverse proxy
// that terminates the control-plane connection all clients share one bucket.
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

func (l *loginRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", strconv.Itoa(int(l.window.Seconds())))
			http.Error(w, "too many login attempts, try again later", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the request's source IP without the port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
