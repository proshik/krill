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
		if err := s.streamServiceLogsToWS(conn.CloseRead(context.Background()), conn, "krill-1"); err != nil {
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
