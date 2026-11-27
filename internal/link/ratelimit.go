package link

import (
	"sync"
	"time"
)

// rateLimiter is a fixed-window per-key limiter, as the login limiter: the
// relay accepts at most limit pairing attempts per source address per window.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*window
	last   time.Time
}

type window struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, w time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: w, hits: map[string]*window{}}
}

func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) >= l.window {
		l.last = now
		for k, w := range l.hits {
			if now.Sub(w.start) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= l.window {
		l.hits[key] = &window{count: 1, start: now}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}
