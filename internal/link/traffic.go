package link

import (
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
)

// trafficKey is what egress traffic is counted by: on the relay a peer's
// service and destination, on a client an app's destination (peer 0).
type trafficKey struct {
	Peer    int64
	Service int64
	Dest    string
}

// trafficCell counts one key since the process started; nothing is stored.
type trafficCell struct {
	up, down atomic.Int64 // bytes towards the destination, and back
	conns    atomic.Int64 // connections let through
	refused  atomic.Int64 // connections refused or failed
}

// Traffic is one key's counters.
type Traffic struct {
	Service int64
	Dest    string
	Up      int64
	Down    int64
	Conns   int64
	Refused int64
}

// traffic holds the counters of one side.
type traffic struct {
	mu sync.Mutex
	m  map[trafficKey]*trafficCell
}

func newTraffic() *traffic { return &traffic{m: map[trafficKey]*trafficCell{}} }

// maxTrafficKeys bounds the counters a side keeps: keys come from the other
// side's labels.
const maxTrafficKeys = 4096

// cell returns the counters of k, creating them; past maxTrafficKeys every new
// key shares one overflow cell.
func (t *traffic) cell(k trafficKey) *trafficCell {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.m[k]
	if c == nil {
		if len(t.m) >= maxTrafficKeys {
			k = trafficKey{Peer: k.Peer, Service: -1, Dest: "other"}
			if c = t.m[k]; c != nil {
				return c
			}
		}
		c = &trafficCell{}
		t.m[k] = c
	}
	return c
}

// of returns the counters of one peer (0 on a client), by service then
// destination.
func (t *traffic) of(peer int64) []Traffic {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Traffic
	for k, c := range t.m {
		if k.Peer != peer {
			continue
		}
		out = append(out, Traffic{Service: k.Service, Dest: k.Dest, Up: c.up.Load(), Down: c.down.Load(),
			Conns: c.conns.Load(), Refused: c.refused.Load()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Dest < out[j].Dest
	})
	return out
}

// countingWriter adds what it writes to n.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// halfClose ends c's sending side: CloseWrite where the connection has one
// (TCP, TLS), Close otherwise (a yamux stream's Close is a half close: it
// keeps reading until the other side closes too).
func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// splice copies a→b and b→a until both directions end, half-closing each
// destination when its source ends, and counts the bytes: a→b into up, b→a
// into down. It closes both connections when it returns, and as soon as
// sessionDone closes: a stream of an ended session reads as a plain EOF,
// and the other side might never close its half.
func splice(a, b net.Conn, up, down *atomic.Int64, sessionDone <-chan struct{}) {
	var wg sync.WaitGroup
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-sessionDone:
			_ = a.Close()
			_ = b.Close()
		case <-finished:
		}
	}()
	wg.Add(2)
	cp := func(dst, src net.Conn, n *atomic.Int64) {
		defer wg.Done()
		_, err := io.Copy(countingWriter{w: dst, n: n}, src)
		if err != nil {
			// A broken direction ends the other one too.
			_ = a.Close()
			_ = b.Close()
			return
		}
		halfClose(dst)
	}
	go cp(b, a, up)
	go cp(a, b, down)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}
