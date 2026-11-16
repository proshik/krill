package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Inflight records the HTTP requests currently being served, so a shutdown
// that runs out of time can say which requests it was still waiting for.
// WebSocket upgrades are left out: http.Server.Shutdown does not wait for
// hijacked connections, so listing them would point at the wrong culprit.
type Inflight struct {
	mu   sync.Mutex
	next uint64
	reqs map[uint64]inflightReq
}

type inflightReq struct {
	method, path string
	started      time.Time
}

// NewInflight returns an empty tracker.
func NewInflight() *Inflight {
	return &Inflight{reqs: make(map[uint64]inflightReq)}
}

// Wrap tracks every request next serves. Only the method and the path are
// kept — never the query string, which may carry something secret.
func (t *Inflight) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}
		t.mu.Lock()
		t.next++
		id := t.next
		t.reqs[id] = inflightReq{method: r.Method, path: r.URL.Path, started: time.Now()}
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			delete(t.reqs, id)
			t.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// Pending describes the requests still being served, oldest first, as
// "METHOD /path (age)".
func (t *Inflight) Pending() []string {
	t.mu.Lock()
	reqs := make([]inflightReq, 0, len(t.reqs))
	for _, r := range t.reqs {
		reqs = append(reqs, r)
	}
	t.mu.Unlock()
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].started.Before(reqs[j].started) })
	out := make([]string, 0, len(reqs))
	now := time.Now()
	for _, r := range reqs {
		out = append(out, fmt.Sprintf("%s %s (%s)", r.method, r.path, now.Sub(r.started).Round(time.Second)))
	}
	return out
}
