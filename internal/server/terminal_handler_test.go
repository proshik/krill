package server

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/proshik/krill/internal/docker"
)

// --- parseResize tests ---

func TestParseResize(t *testing.T) {
	cases := []struct {
		in     string
		wantR  uint
		wantC  uint
		wantOK bool
	}{
		{`{"rows":40,"cols":120}`, 40, 120, true},
		{`{"rows":1,"cols":1}`, 1, 1, true},
		{`{"rows":1000,"cols":1000}`, 1000, 1000, true}, // cap boundary (inclusive)
		{`{"rows":1001,"cols":80}`, 0, 0, false},        // just over cap
		{`{"rows":0,"cols":80}`, 0, 0, false},
		{`{"rows":40,"cols":0}`, 0, 0, false},
		{`{"rows":-1,"cols":80}`, 0, 0, false}, // negative → unmarshal error
		{`{"rows":99999,"cols":80}`, 0, 0, false},
		{`not json`, 0, 0, false},
		{`{}`, 0, 0, false},
	}
	for _, tc := range cases {
		r, c, ok := parseResize([]byte(tc.in))
		if ok != tc.wantOK || (ok && (r != tc.wantR || c != tc.wantC)) {
			t.Errorf("parseResize(%q) = (%d,%d,%v), want (%d,%d,%v)", tc.in, r, c, ok, tc.wantR, tc.wantC, tc.wantOK)
		}
	}
}

// --- pumpTerminal fakes ---

type wsFrame struct {
	typ  websocket.MessageType
	data []byte
}

type fakeWS struct {
	in      chan wsFrame
	mu      sync.Mutex
	written []wsFrame
	closed  bool
}

func newFakeWS() *fakeWS { return &fakeWS{in: make(chan wsFrame, 8)} }

func (w *fakeWS) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case f, ok := <-w.in:
		if !ok {
			return 0, nil, io.EOF
		}
		return f.typ, f.data, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}
func (w *fakeWS) Write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	if ctx.Err() != nil {
		return ctx.Err() // mirror *websocket.Conn: no writes after the ctx is cancelled
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = append(w.written, wsFrame{typ, append([]byte(nil), data...)})
	return nil
}
func (w *fakeWS) Close(websocket.StatusCode, string) error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return nil
}
func (w *fakeWS) writes() []wsFrame { w.mu.Lock(); defer w.mu.Unlock(); return append([]wsFrame(nil), w.written...) }
func (w *fakeWS) isClosed() bool    { w.mu.Lock(); defer w.mu.Unlock(); return w.closed }

type fakeSession struct {
	mu      sync.Mutex
	stdin   []byte
	resizes [][2]uint
	out     chan []byte
	closed  chan struct{}
	once    sync.Once
}

func newFakeSession() *fakeSession {
	return &fakeSession{out: make(chan []byte, 8), closed: make(chan struct{})}
}
func (s *fakeSession) Read(p []byte) (int, error) {
	select {
	case b := <-s.out:
		return copy(p, b), nil
	case <-s.closed:
		return 0, io.EOF
	}
}
func (s *fakeSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stdin = append(s.stdin, p...)
	return len(p), nil
}
func (s *fakeSession) Resize(_ context.Context, rows, cols uint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resizes = append(s.resizes, [2]uint{rows, cols})
	return nil
}
func (s *fakeSession) Close() error { s.once.Do(func() { close(s.closed) }); return nil }
func (s *fakeSession) stdinBytes() []byte     { s.mu.Lock(); defer s.mu.Unlock(); return append([]byte(nil), s.stdin...) }
func (s *fakeSession) resizeCalls() [][2]uint { s.mu.Lock(); defer s.mu.Unlock(); return append([][2]uint(nil), s.resizes...) }
func (s *fakeSession) isClosedHelper() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

var _ docker.ExecSession = (*fakeSession)(nil)

func waitForCond(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 1s")
}

// --- pumpTerminal tests ---

func TestPumpForwardsStdinAndResize(t *testing.T) {
	ws := newFakeWS()
	sess := newFakeSession()
	done := make(chan struct{})
	go func() { pumpTerminal(context.Background(), ws, sess, time.Minute); close(done) }()

	ws.in <- wsFrame{websocket.MessageBinary, []byte("ls\n")}
	ws.in <- wsFrame{websocket.MessageText, []byte(`{"rows":40,"cols":120}`)}
	waitForCond(t, func() bool { return string(sess.stdinBytes()) == "ls\n" && len(sess.resizeCalls()) == 1 })

	close(ws.in)
	<-done
	if got := sess.resizeCalls(); got[0] != [2]uint{40, 120} {
		t.Fatalf("resize = %v, want [40 120]", got[0])
	}
	if !sess.isClosedHelper() {
		t.Fatal("session not closed on WS close")
	}
	if !ws.isClosed() {
		t.Fatal("ws not closed on shutdown")
	}
}

func TestPumpForwardsOutput(t *testing.T) {
	ws := newFakeWS()
	sess := newFakeSession()
	done := make(chan struct{})
	go func() { pumpTerminal(context.Background(), ws, sess, time.Minute); close(done) }()

	sess.out <- []byte("hello")
	waitForCond(t, func() bool {
		for _, f := range ws.writes() {
			if f.typ == websocket.MessageBinary && string(f.data) == "hello" {
				return true
			}
		}
		return false
	})
	close(ws.in)
	<-done
}

func TestPumpIdleTimeoutCloses(t *testing.T) {
	ws := newFakeWS()
	sess := newFakeSession()
	done := make(chan struct{})
	go func() { pumpTerminal(context.Background(), ws, sess, 40*time.Millisecond); close(done) }()
	<-done
	if !sess.isClosedHelper() || !ws.isClosed() {
		t.Fatalf("idle timeout should close session and ws (sess=%v ws=%v)", sess.isClosedHelper(), ws.isClosed())
	}
}
