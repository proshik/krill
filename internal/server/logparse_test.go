package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/logparse"
)

// taskLogsEngine answers ServiceLogs the way Swarm does: one task's lines
// after another's, not by time. The following stream repeats the tail.
type taskLogsEngine struct {
	docker.Engine
	backlog, live string
	tails         []int
}

func (e *taskLogsEngine) ServiceLogs(_ context.Context, _ string, follow bool, tail int) (io.ReadCloser, error) {
	e.tails = append(e.tails, tail)
	if follow {
		return io.NopCloser(strings.NewReader(e.live)), nil
	}
	return io.NopCloser(strings.NewReader(e.backlog)), nil
}

// The logs tab of an app redeployed days ago showed the stopped task's lines
// last, below the running task's. The viewer must get the running task's
// newest line last, and the live stream must not repeat the backlog.
func TestServiceLogsStreamNewestLast(t *testing.T) {
	eng := &taskLogsEngine{
		backlog: "2026-11-23T10:00:01Z current 1\n" +
			"2026-11-23T10:00:02Z current 2\n" +
			"2026-11-19T08:00:01Z stopped 1\n" +
			"2026-11-19T08:00:01Z \n" +
			"2026-11-19T08:00:02Z stopped 2\n",
		live: "2026-11-23T10:00:02Z current 2\n" +
			"2026-11-19T08:00:02Z stopped 2\n" +
			"2026-11-23T10:00:03Z current 3\n",
	}
	s := &Server{engine: eng}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if err := s.streamServiceLogsToWS(conn.CloseRead(context.Background()), conn, "krill-1", "GAP"); err != nil {
			t.Errorf("stream: %v", err)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var got []string
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("read: %v", err)
			}
			break
		}
		var l logparse.LogLine
		if err := json.Unmarshal(b, &l); err != nil {
			t.Fatal(err)
		}
		got = append(got, l.Msg)
	}
	want := "stopped 1|stopped 2|current 1|current 2|current 3"
	if strings.Join(got, "|") != want {
		t.Fatalf("lines = %q, want %q", strings.Join(got, "|"), want)
	}
	if len(eng.tails) != 2 || eng.tails[0] != logWindow || eng.tails[1] != liveOverlap {
		t.Fatalf("tails = %v, want [%d %d]", eng.tails, logWindow, liveOverlap)
	}
}

// A task that wrote more than the overlap between the backlog and the live
// stream: its first following line is already new, and the viewer is told
// that lines may be missing before it.
func TestServiceLogsStreamReportsGap(t *testing.T) {
	eng := &taskLogsEngine{
		backlog: "2026-11-23T10:00:01Z com.docker.swarm.task.id=a a1\n",
		live:    "2026-11-23T10:00:09Z com.docker.swarm.task.id=a a9\n2026-11-23T10:00:10Z com.docker.swarm.task.id=a a10\n",
	}
	s := &Server{engine: eng}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = s.streamServiceLogsToWS(conn.CloseRead(context.Background()), conn, "krill-1", "GAP")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var got []string
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var l logparse.LogLine
		_ = json.Unmarshal(b, &l)
		got = append(got, l.Msg)
	}
	if strings.Join(got, "|") != "a1|GAP|a9|a10" {
		t.Fatalf("lines = %q, want the gap notice once, before the first new line", strings.Join(got, "|"))
	}
}

// followUntilGone is a live log stream that stays open until the viewer
// leaves, then fails the way docker's does: with the context's error.
type followUntilGone struct {
	docker.Engine
	backlogErr bool // the backlog read already sees the viewer gone
}

func (e *followUntilGone) ServiceLogs(ctx context.Context, _ string, follow bool, _ int) (io.ReadCloser, error) {
	if !follow {
		if e.backlogErr {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return io.NopCloser(strings.NewReader("2026-11-23T10:00:01Z hello\n")), nil
	}
	pr, pw := io.Pipe()
	go func() { <-ctx.Done(); pw.CloseWithError(ctx.Err()) }()
	return pr, nil
}

// Leaving a logs tab closes its WebSocket, which cancels the stream's
// context; that ended every stream with an ERROR "context canceled" line in
// the journal (seen on v0.13.1). A viewer leaving is the normal end.
func TestServiceLogsViewerLeavingIsNotAnError(t *testing.T) {
	for _, backlogErr := range []bool{false, true} {
		var buf strings.Builder
		var mu sync.Mutex
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
		t.Cleanup(func() { slog.SetDefault(old) })

		s := &Server{engine: &followUntilGone{backlogErr: backlogErr}}
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			s.serveServiceLogs(w, r, "krill-1")
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !backlogErr {
			if _, _, err := conn.Read(ctx); err != nil {
				t.Fatalf("read backlog: %v", err)
			}
		}
		conn.Close(websocket.StatusGoingAway, "")
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not end when the viewer left")
		}
		cancel()
		srv.Close()
		mu.Lock()
		out := buf.String()
		mu.Unlock()
		if strings.Contains(out, "level=ERROR") {
			t.Fatalf("backlogErr=%v: viewer leaving logged an error:\n%s", backlogErr, out)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// brokenFollow is a live log stream that breaks while the viewer is still
// there: the daemon's connection drops mid-stream.
type brokenFollow struct{ docker.Engine }

func (brokenFollow) ServiceLogs(_ context.Context, _ string, follow bool, _ int) (io.ReadCloser, error) {
	if !follow {
		return io.NopCloser(strings.NewReader("2026-11-23T10:00:01Z hello\n")), nil
	}
	return io.NopCloser(io.MultiReader(strings.NewReader("2026-11-23T10:00:02Z more\n"), errReader{io.ErrUnexpectedEOF})), nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// A stream that breaks while the viewer is connected is still an error: it is
// logged and the viewer is told (the quiet close is for a viewer leaving).
func TestServiceLogsBrokenStreamIsStillAnError(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	s := &Server{engine: brokenFollow{}}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		s.serveServiceLogs(w, r, "krill-1")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var last logparse.LogLine
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
				t.Fatalf("close status = %v, want internal error", got)
			}
			break
		}
		_ = json.Unmarshal(b, &last)
	}
	<-done
	if last.Level != "error" {
		t.Fatalf("the viewer was not told the stream broke: last line %+v", last)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "unexpected EOF") {
		t.Fatalf("broken stream not logged as an error:\n%s", out)
	}
}
