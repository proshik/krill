package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/proshik/krill/internal/logparse"
)

const (
	// logWindow is how many of the newest lines the viewer starts with,
	// across all of the service's tasks.
	logWindow = 200
	// liveOverlap is the per-task tail of the following stream, opened right
	// after the backlog was read: it covers lines written between the two
	// reads, and the cursor drops the ones the backlog already showed.
	liveOverlap = 50
)

// streamServiceLogsToWS sends a service's runtime log over conn: first its
// newest lines merged across tasks by time (see logparse.Backlog), then the
// live stream. An error opening the backlog is returned before anything is
// sent, so the caller can log it and close the connection; later failures
// close the connection here.
func (s *Server) streamServiceLogsToWS(ctx context.Context, conn *websocket.Conn, service string) error {
	rc, err := s.engine.ServiceLogs(ctx, service, false, logWindow)
	if err != nil {
		return err
	}
	var backlog []logparse.LogLine
	var scanErr error
	if rc != nil {
		backlog, scanErr = logparse.Backlog(rc, logWindow)
		rc.Close()
	}
	for _, l := range backlog {
		if !writeLogLine(ctx, conn, l) {
			return nil
		}
	}
	if endWithNotice(ctx, conn, scanErr) {
		return nil
	}

	live, err := s.engine.ServiceLogs(ctx, service, true, liveOverlap)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("log stream: follow failed", "err", err, "service", service)
		}
		conn.Close(websocket.StatusInternalError, "logs unavailable")
		return nil
	}
	if live == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil
	}
	defer live.Close()
	streamParsedLogsToWS(ctx, conn, live, logparse.NewCursor(backlog).Fresh)
	return nil
}

// streamParsedLogsToWS reads lines from rc, parses each into a LogLine, and
// sends the ones keep accepts as JSON text frames until the reader is
// exhausted or a write fails.
func streamParsedLogsToWS(ctx context.Context, conn *websocket.Conn, rc io.Reader, keep func(logparse.LogLine) bool) {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		l := logparse.ParseLogLine(sc.Text())
		if keep != nil && !keep(l) {
			continue
		}
		if !writeLogLine(ctx, conn, l) {
			return
		}
	}
	if endWithNotice(ctx, conn, sc.Err()) {
		return
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

// writeLogLine sends one line; false means the connection is gone.
func writeLogLine(ctx context.Context, conn *websocket.Conn, l logparse.LogLine) bool {
	b, err := json.Marshal(l)
	if err != nil {
		return true
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b) == nil
}

// endWithNotice sends the reader a notice and closes conn when scanErr says
// the stream broke; it reports whether it did.
func endWithNotice(ctx context.Context, conn *websocket.Conn, scanErr error) bool {
	notice, ok := logparse.ScanEndNotice(scanErr)
	if !ok {
		return false
	}
	// The notice handed to the reader is generic; the cause belongs here.
	slog.Error("log stream scan ended with an error", "err", scanErr)
	writeLogLine(ctx, conn, notice)
	conn.Close(websocket.StatusInternalError, "log stream broken")
	return true
}
