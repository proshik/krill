package link

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// When the session ends, splice closes both sides even though neither side
// closed: a stream of an ended session reads as EOF, and the destination may
// keep its half open and silent for ever.
func TestSpliceEndsWithTheSession(t *testing.T) {
	a, a2 := net.Pipe()
	b, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	done := make(chan struct{})
	ended := make(chan struct{})
	var up, down atomic.Int64
	go func() { splice(a, b, &up, &down, done); close(ended) }()
	select {
	case <-ended:
		t.Fatal("splice ended before anything closed")
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("splice outlived its session")
	}
}
