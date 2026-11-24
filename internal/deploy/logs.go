package deploy

import (
	"fmt"
	"strings"
	"sync"
)

const (
	logHeadCap = 128 * 1024
	logTailCap = 128 * 1024
)

// DeployLogHub holds logs of in-progress deployments: buffer + live subscribers.
type DeployLogHub struct {
	mu    sync.Mutex
	feeds map[int64]*feed
}

type feed struct {
	head    []byte
	tail    []byte // last logTailCap bytes once head is full
	dropped int    // bytes evicted from the middle
	subs    map[chan string]struct{}
}

func (f *feed) append(b []byte) {
	if len(f.head) < logHeadCap {
		room := logHeadCap - len(f.head)
		if room >= len(b) {
			f.head = append(f.head, b...)
			return
		}
		f.head = append(f.head, b[:room]...)
		b = b[room:]
	}
	f.tail = append(f.tail, b...)
	if len(f.tail) > logTailCap {
		over := len(f.tail) - logTailCap
		f.dropped += over
		f.tail = f.tail[over:]
	}
}

func (f *feed) snapshot() string {
	if f.dropped == 0 {
		// head+tail is the complete, contiguous log — no marker.
		return storable(string(f.head) + string(f.tail))
	}
	return storable(string(f.head) + fmt.Sprintf("\n...[truncated %d bytes]...\n", f.dropped) + string(f.tail))
}

// storable makes a log something PostgreSQL's TEXT accepts. It refuses the
// whole UPDATE for invalid UTF-8 — which the byte caps above produce
// whenever a cut falls inside a multi-byte character — and for a NUL byte,
// which a build or predeploy command can print; the deployment's status went
// unrecorded with it.
func storable(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

func (f *feed) hasContent() bool { return len(f.head) > 0 || len(f.tail) > 0 }

func NewLogHub() *DeployLogHub {
	return &DeployLogHub{feeds: map[int64]*feed{}}
}

// Open registers a deployment.
func (h *DeployLogHub) Open(deployID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.feeds[deployID]; !ok {
		h.feeds[deployID] = &feed{subs: map[chan string]struct{}{}}
	}
}

// Writer returns an io.Writer for the deployment (writes to the buffer and broadcasts to subscribers).
func (h *DeployLogHub) Writer(deployID int64) *hubWriter {
	return &hubWriter{hub: h, deployID: deployID}
}

type hubWriter struct {
	hub      *DeployLogHub
	deployID int64
}

func (w *hubWriter) Write(p []byte) (int, error) {
	w.hub.mu.Lock()
	defer w.hub.mu.Unlock()
	f := w.hub.feeds[w.deployID]
	if f == nil {
		return len(p), nil // deployment already closed — silently ignore
	}
	s := string(p)
	f.append(p)
	for ch := range f.subs {
		select {
		case ch <- s:
		default: // slow subscriber — don't block the worker
		}
	}
	return len(p), nil
}

// maxSubsPerFeed caps live subscribers on a single deployment's log feed so a
// member cannot pin unbounded goroutines + 256-slot channels by opening many
// sockets to the same feed.
const maxSubsPerFeed = 16

// Subscribe subscribes; immediately receives the accumulated buffer as the first message.
func (h *DeployLogHub) Subscribe(deployID int64) chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan string, 256)
	f := h.feeds[deployID]
	if f == nil || len(f.subs) >= maxSubsPerFeed {
		close(ch) // not in progress, or this feed already has too many subscribers
		return ch
	}
	if f.hasContent() {
		ch <- f.snapshot()
	}
	f.subs[ch] = struct{}{}
	return ch
}

// Unsubscribe removes a subscriber.
func (h *DeployLogHub) Unsubscribe(deployID int64, ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f := h.feeds[deployID]; f != nil {
		if _, ok := f.subs[ch]; ok {
			delete(f.subs, ch)
			close(ch)
		}
	}
}

// Close closes the deployment: returns the full log, closes subscribers, and removes the feed.
func (h *DeployLogHub) Close(deployID int64) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.feeds[deployID]
	if f == nil {
		return ""
	}
	for ch := range f.subs {
		close(ch)
	}
	full := f.snapshot()
	delete(h.feeds, deployID)
	return full
}

// Active reports whether a deployment is in progress (feed exists).
func (h *DeployLogHub) Active(deployID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.feeds[deployID]
	return ok
}
