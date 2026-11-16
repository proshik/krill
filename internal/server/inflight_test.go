package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pendingSoon polls Pending until want returns true or a second passes.
func pendingSoon(t *testing.T, tr *Inflight, want func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got := tr.Pending()
		if want(got) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInflightPending(t *testing.T) {
	tr := NewInflight()
	entered := make(chan string, 4)
	release := make(chan struct{})
	ts := httptest.NewUnstartedServer(tr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.URL.Path
		if r.URL.Path == "/hijack" {
			c, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer c.Close()
		}
		<-release
	})))
	ts.Config.ConnContext = tr.ConnContext
	ts.Config.ConnState = tr.ConnState
	ts.Start()
	defer ts.Close()
	defer close(release)

	send := func(path string, header string) {
		c, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n%s\r\n", path, header)
		go func() { _, _ = bufio.NewReader(c).ReadString('\n') }()
		if got := <-entered; got != strings.SplitN(path, "?", 2)[0] {
			t.Fatalf("entered %q, want %q", got, path)
		}
	}

	// An ordinary request is listed while served, without its query string.
	send("/plain?token=secret", "")
	got := pendingSoon(t, tr, func(p []string) bool { return len(p) == 1 })
	if len(got) != 1 || !strings.HasPrefix(got[0], "GET /plain (") {
		t.Fatalf("pending while serving: %q", got)
	}
	if strings.Contains(got[0], "secret") {
		t.Fatalf("the query string leaked into the description: %q", got[0])
	}

	// An upgrade request that has not been hijacked yet holds Shutdown, so it
	// is listed like any other.
	send("/upgrade", "Upgrade: websocket\r\nConnection: Upgrade\r\n")
	got = pendingSoon(t, tr, func(p []string) bool { return len(p) == 2 })
	if len(got) != 2 || !strings.HasPrefix(got[1], "GET /upgrade (") {
		t.Fatalf("an upgrade request before its hijack is not listed: %q", got)
	}

	// Once hijacked, Shutdown stops waiting for the connection, and the
	// request leaves the list even though its handler is still running.
	send("/hijack", "Upgrade: websocket\r\nConnection: Upgrade\r\n")
	got = pendingSoon(t, tr, func(p []string) bool {
		for _, s := range p {
			if strings.Contains(s, "/hijack") {
				return false
			}
		}
		return len(p) == 2
	})
	for _, s := range got {
		if strings.Contains(s, "/hijack") {
			t.Fatalf("a hijacked request is listed as pending: %q", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("pending after the hijack: %q", got)
	}
}

func TestInflightForgetsFinishedRequests(t *testing.T) {
	tr := NewInflight()
	h := tr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if p := tr.Pending(); len(p) != 0 {
		t.Fatalf("a finished request is still pending: %q", p)
	}
}

// After the handler returns, the server still reads the rest of a body the
// handler left unread, and the connection stays active — Shutdown keeps
// waiting for it, so the request stays listed until the connection is done.
func TestInflightKeepsRequestWhileItsBodyIsDrained(t *testing.T) {
	tr := NewInflight()
	returned := make(chan struct{}, 1)
	ts := httptest.NewUnstartedServer(tr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // answers without reading the body
		returned <- struct{}{}
	})))
	ts.Config.ConnContext = tr.ConnContext
	ts.Config.ConnState = tr.ConnState
	ts.Start()
	defer ts.Close()

	c, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "POST /unread HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nx") // 99 bytes never sent
	<-returned
	time.Sleep(100 * time.Millisecond) // the server is now draining the body
	got := tr.Pending()
	if len(got) != 1 || !strings.HasPrefix(got[0], "POST /unread (") || !strings.HasSuffix(got[0], ", handler done)") {
		t.Fatalf("a request whose connection is still draining its body: %q", got)
	}

	c.Close()
	if got := pendingSoon(t, tr, func(p []string) bool { return len(p) == 0 }); len(got) != 0 {
		t.Fatalf("the request is still listed after its connection closed: %q", got)
	}
}

// A keep-alive connection that goes idle after a complete request releases it.
func TestInflightReleasesRequestWhenConnectionGoesIdle(t *testing.T) {
	tr := NewInflight()
	ts := httptest.NewUnstartedServer(tr.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	ts.Config.ConnContext = tr.ConnContext
	ts.Config.ConnState = tr.ConnState
	ts.Start()
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/done")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // the connection stays open, idle
	if got := pendingSoon(t, tr, func(p []string) bool { return len(p) == 0 }); len(got) != 0 {
		t.Fatalf("a finished request on an idle connection is still listed: %q", got)
	}
}
