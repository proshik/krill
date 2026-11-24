package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/logparse"
)

var testNotices = logNotices{Gap: "GAP", Cut: "CUT"}

// stallingLogsEngine's backlog read gives its text and then never ends, the
// way Swarm's did for two apps on v0.13.2; its live stream gives live and
// then stays open until the viewer leaves.
type stallingLogsEngine struct {
	docker.Engine
	backlog, live string
	stall         bool // the backlog read stalls instead of ending
}

func (e *stallingLogsEngine) ServiceLogs(ctx context.Context, _ string, follow bool, _ int) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	text := e.backlog
	if follow {
		text = e.live
	}
	go func() {
		pw.Write([]byte(text))
		if !follow && !e.stall {
			pw.Close()
			return
		}
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	return pr, nil
}

// viewLogs connects a viewer to eng's stream and returns every frame it got
// within the wait ("EV" for the live event), and how long the first frame
// took.
func viewLogs(t *testing.T, eng docker.Engine, frames int) ([]string, time.Duration) {
	t.Helper()
	old := backlogTimeout
	backlogTimeout = 200 * time.Millisecond
	t.Cleanup(func() { backlogTimeout = old })

	s := &Server{engine: eng}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = s.streamServiceLogsToWS(conn.CloseRead(context.Background()), conn, "krill-1", testNotices)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var got []string
	var first time.Duration
	for len(got) < frames {
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("after %q: %v", got, err)
		}
		if first == 0 {
			first = time.Since(start)
		}
		if string(b) == string(liveEvent) {
			got = append(got, "EV")
			continue
		}
		var l logparse.LogLine
		if err := json.Unmarshal(b, &l); err != nil {
			t.Fatal(err)
		}
		got = append(got, l.Msg)
	}
	return got, first
}

// The backlog read prints lines and then never ends: the viewer gets those
// lines (merged by time), the notice that the history is incomplete, and
// then the live stream, with the replay of what it already showed dropped.
func TestServiceLogsStalledBacklogStillShows(t *testing.T) {
	eng := &stallingLogsEngine{
		stall: true,
		backlog: "2026-11-24T10:00:01Z com.docker.swarm.task.id=a a1\n" +
			"2026-11-24T10:00:02Z com.docker.swarm.task.id=a a2\n" +
			"2026-11-20T08:00:01Z com.docker.swarm.task.id=old old1\n",
		live: "2026-11-24T10:00:01Z com.docker.swarm.task.id=a a1\n" +
			"2026-11-24T10:00:02Z com.docker.swarm.task.id=a a2\n" +
			"2026-11-20T08:00:01Z com.docker.swarm.task.id=old old1\n" +
			"2026-11-24T10:00:03Z com.docker.swarm.task.id=a a3\n",
	}
	got, first := viewLogs(t, eng, 6)
	if want := "old1|a1|a2|CUT|EV|a3"; strings.Join(got, "|") != want {
		t.Fatalf("frames = %q, want %q", strings.Join(got, "|"), want)
	}
	if first > 3*time.Second {
		t.Fatalf("first frame after %v", first)
	}
}

// A backlog that stalls before its first line: the viewer is told the
// history is incomplete and that the live stream is on (it shows the waiting
// text), then gets the live lines.
func TestServiceLogsStalledEmptyBacklog(t *testing.T) {
	eng := &stallingLogsEngine{
		stall: true,
		live:  "2026-11-24T10:00:03Z com.docker.swarm.task.id=a a3\n",
	}
	got, _ := viewLogs(t, eng, 3)
	if want := "CUT|EV|a3"; strings.Join(got, "|") != want {
		t.Fatalf("frames = %q, want %q", strings.Join(got, "|"), want)
	}
}

// An empty backlog that ends normally: no notice, just the live event, so
// the viewer shows the waiting text instead of a blank pane.
func TestServiceLogsEmptyBacklogSaysLive(t *testing.T) {
	eng := &stallingLogsEngine{live: "2026-11-24T10:00:03Z com.docker.swarm.task.id=a a3\n"}
	got, _ := viewLogs(t, eng, 2)
	if want := "EV|a3"; strings.Join(got, "|") != want {
		t.Fatalf("frames = %q, want %q", strings.Join(got, "|"), want)
	}
}
