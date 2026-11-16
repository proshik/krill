package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/server"
)

// syncBuffer is a bytes.Buffer safe to write from slog and read from the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func startServer(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + ln.Addr().String()
}

// TestGracefulShutdownExpiredBudgetIsNotFatal: a request that ignores the
// shutdown must not turn a stop into a failure. The budget runs out, the
// connection is closed forcibly, the request is named in a WARN, and the
// function returns — run() then returns nil, so the process exits 0.
func TestGracefulShutdownExpiredBudgetIsNotFatal(t *testing.T) {
	logs := captureLogs(t)
	inflight := server.NewInflight()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv, url := startServer(t, inflight.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // ignores its context, like a stream that never notices the shutdown
	})))

	clientErr := make(chan error, 1)
	go func() {
		resp, err := http.Get(url + "/stuck")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		clientErr <- err
	}()
	<-entered

	stopped := false
	start := time.Now()
	gracefulShutdown(srv, 200*time.Millisecond, inflight, func(context.Context) error { stopped = true; return nil })
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("gracefulShutdown took %s with a 200ms budget", d)
	}
	if !stopped {
		t.Fatal("background work was not stopped")
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "GET /stuck") {
		t.Fatalf("want a WARN naming the pending request, got:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("an expired budget must not log an error:\n%s", out)
	}
	select {
	case err := <-clientErr:
		if err == nil {
			t.Fatal("the stuck request should have been cut off")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stuck connection was not closed")
	}
}

func TestGracefulShutdownClean(t *testing.T) {
	logs := captureLogs(t)
	inflight := server.NewInflight()
	srv, url := startServer(t, inflight.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	resp, err := http.Get(url + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	workErr := errors.New("clone work still running")
	gracefulShutdown(srv, time.Second, inflight, func(context.Context) error { return workErr })
	out := logs.String()
	if !strings.Contains(out, "shutdown complete") {
		t.Fatalf("want a clean shutdown, got:\n%s", out)
	}
	if !strings.Contains(out, "environment clone work did not stop") {
		t.Fatalf("a stopWork error should be logged, got:\n%s", out)
	}
	if _, err := http.Get(url + "/"); err == nil {
		t.Fatal("the server still accepts requests after shutdown")
	}
}
