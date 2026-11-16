package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Inflight records the HTTP requests currently being served, so a shutdown
// that runs out of time can say which requests it was still waiting for.
// http.Server.Shutdown waits for a connection while it is active, which is
// not quite while its handler runs, so the tracker follows the connection:
//   - a request leaves the list once its connection is hijacked (an accepted
//     WebSocket) — Shutdown does not wait for hijacked connections, so listing
//     them would point at the wrong culprit. Until the hijack actually happens
//     (an upgrade request still in authentication, say) it holds Shutdown and
//     stays listed;
//   - after the handler returns, the server may still be reading the rest of a
//     request body the handler left unread, and the connection stays active
//     until it goes idle or closes; the request stays listed until then.
//
// (A connection still sending its request line or headers is not active yet;
// Shutdown closes it on its own.) Wire ConnContext and ConnState into the
// server; without them a request leaves the list when its handler returns.
type Inflight struct {
	mu   sync.Mutex
	next uint64
	reqs map[uint64]inflightReq
}

type inflightReq struct {
	method, path string
	started      time.Time
	conn         net.Conn
	handled      bool // the handler returned; the connection is still active
}

type inflightConnKey struct{}

// NewInflight returns an empty tracker.
func NewInflight() *Inflight {
	return &Inflight{reqs: make(map[uint64]inflightReq)}
}

// ConnContext is an http.Server.ConnContext that remembers each request's
// connection, so ConnState can tell which requests it is done with.
func (t *Inflight) ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, inflightConnKey{}, c)
}

// ConnState is an http.Server.ConnState that drops the request of a
// connection once the connection goes idle, closes or is hijacked — the
// points from which Shutdown no longer waits for it. Closed and hijacked are
// terminal states, so nothing is kept for such a connection.
func (t *Inflight) ConnState(c net.Conn, st http.ConnState) {
	if st != http.StateIdle && st != http.StateClosed && st != http.StateHijacked {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, r := range t.reqs {
		if r.conn == c {
			delete(t.reqs, id)
		}
	}
}

// Wrap tracks every request next serves. Only the method and the path are
// kept — never the query string, which may carry something secret.
func (t *Inflight) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Context().Value(inflightConnKey{}).(net.Conn)
		t.mu.Lock()
		t.next++
		id := t.next
		t.reqs[id] = inflightReq{method: r.Method, path: r.URL.Path, started: time.Now(), conn: c}
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if c == nil {
				delete(t.reqs, id)
			} else if req, ok := t.reqs[id]; ok {
				req.handled = true
				t.reqs[id] = req
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Pending describes the requests still being served, oldest first, as
// "METHOD /path (age)", or "METHOD /path (age, handler done)" for one whose
// connection is still busy after its handler returned.
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
		note := ""
		if r.handled {
			note = ", handler done"
		}
		out = append(out, fmt.Sprintf("%s %s (%s%s)", r.method, r.path, now.Sub(r.started).Round(time.Second), note))
	}
	return out
}
