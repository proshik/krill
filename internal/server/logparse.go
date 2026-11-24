package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	// liveOverlap is the per-task tail of the following stream, opened as
	// soon as the backlog is read: it covers lines written between the two
	// reads, and the cursor drops the ones the backlog already read.
	liveOverlap = 50
)

// logNotices are the viewer's texts for the two ways its log can be
// incomplete, in the viewer's language.
type logNotices struct {
	// Gap goes before a task's first following line when the task wrote
	// more than liveOverlap lines between the two reads.
	Gap string
	// Cut follows the backlog when its read was given up after
	// logparse.BacklogTimeout: some of the earlier lines may be missing, and
	// tasks it never reached show their tail below, from the live stream.
	Cut string
}

// liveEvent is the frame sent once the backlog is through, so the viewer can
// tell a log with no lines yet from one still loading.
var liveEvent = []byte(`{"ev":"live"}`)

// backlogTimeout and followOpenWait are vars so tests can shorten them.
var (
	backlogTimeout = logparse.BacklogTimeout
	// followOpenWait is how long the backlog waits for the live stream to
	// open before it is sent anyway.
	followOpenWait = 5 * time.Second
)

// streamServiceLogsToWS sends a service's runtime log over conn: first its
// newest lines merged across tasks by time (see logparse.Backlog), then the
// live stream. The backlog read, opening included, is given up after
// backlogTimeout, keeping what it read; an incomplete backlog is followed by
// notices.Cut. The live stream is opened right after the backlog is read and,
// unless it takes longer than followOpenWait, before any of it is sent, so a
// slow browser does not widen the gap between the two; should a task still
// write more than liveOverlap lines in between, notices.Gap is sent before
// its first new line. Once the backlog is through, liveEvent tells the viewer
// whatever comes next is live. An error opening the backlog is returned
// before anything is sent, so the caller can log it and close the
// connection; later failures close the connection here.
func (s *Server) streamServiceLogsToWS(ctx context.Context, conn *websocket.Conn, service string, notices logNotices) error {
	backlog, cursor, cut, openErr, scanErr := logparse.ReadBacklog(ctx, backlogTimeout, logWindow,
		func(ctx context.Context) (io.ReadCloser, error) {
			return s.engine.ServiceLogs(ctx, service, false, logWindow)
		})
	if openErr != nil {
		return openErr
	}
	if cut {
		slog.Warn("log stream: backlog incomplete, continuing with what it read",
			"service", service, "timeout", backlogTimeout, "lines", len(backlog))
	}
	var liveCh chan openedLog
	var live io.ReadCloser
	var liveErr error
	waiting := false // the live stream's open is still outstanding
	if scanErr == nil {
		liveCh = make(chan openedLog, 1)
		go func() {
			rc, err := s.engine.ServiceLogs(ctx, service, true, liveOverlap)
			liveCh <- openedLog{rc, err}
		}()
		waiting = true
		t := time.NewTimer(followOpenWait)
		select {
		case o := <-liveCh:
			live, liveErr, waiting = o.rc, o.err, false
		case <-t.C:
		}
		t.Stop()
	}
	defer func() {
		if live != nil {
			live.Close()
		}
		if waiting { // whatever the open still returns is closed
			go func() {
				if o := <-liveCh; o.rc != nil {
					o.rc.Close()
				}
			}()
		}
	}()
	for _, l := range backlog {
		if !writeLogLine(ctx, conn, l) {
			return nil
		}
	}
	if endWithNotice(ctx, conn, scanErr) {
		return nil
	}
	if cut && !writeLogLine(ctx, conn, logparse.LogLine{Level: "warn", Msg: notices.Cut}) {
		return nil
	}
	if !writeFrame(ctx, conn, liveEvent) {
		return nil
	}
	if waiting {
		select {
		case o := <-liveCh:
			live, liveErr, waiting = o.rc, o.err, false
		case <-ctx.Done():
			return nil
		}
	}
	if liveErr != nil {
		if ctx.Err() == nil {
			slog.Error("log stream: follow failed", "err", liveErr, "service", service)
		}
		conn.Close(websocket.StatusInternalError, "logs unavailable")
		return nil
	}
	if live == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		return nil
	}
	streamParsedLogsToWS(ctx, conn, live, func(l logparse.LogLine) bool {
		if !cursor.Fresh(l) {
			return false
		}
		if cursor.Gap() && !writeLogLine(ctx, conn, logparse.LogLine{Level: "warn", Msg: notices.Gap}) {
			return false
		}
		return true
	})
	return nil
}

// openedLog is the outcome of opening a log stream.
type openedLog struct {
	rc  io.ReadCloser
	err error
}

// streamParsedLogsToWS reads lines from rc, parses each into a LogLine, and
// sends the ones keep accepts as JSON text frames until the reader is
// exhausted or a write fails.
func streamParsedLogsToWS(ctx context.Context, conn *websocket.Conn, rc io.Reader, keep func(logparse.LogLine) bool) {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		l := logparse.ParseLogLine(sc.Text())
		// keep sees blank lines too: they are part of a task's replay.
		if (keep != nil && !keep(l)) || logparse.Blank(l) {
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
	return writeFrame(ctx, conn, b)
}

// writeFrame sends one text frame; false means the connection is gone.
func writeFrame(ctx context.Context, conn *websocket.Conn, b []byte) bool {
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b) == nil
}

// endWithNotice sends the reader a notice and closes conn when scanErr says
// the stream broke; it reports whether it did. A stream ended because the
// viewer left (the tab closed or navigated away) is not broken: it is closed
// quietly.
func endWithNotice(ctx context.Context, conn *websocket.Conn, scanErr error) bool {
	notice, ok := logparse.ScanEndNotice(scanErr)
	if !ok {
		return false
	}
	if viewerGone(ctx, scanErr) {
		conn.Close(websocket.StatusNormalClosure, "")
		return true
	}
	// The notice handed to the reader is generic; the cause belongs here.
	slog.Error("log stream scan ended with an error", "err", scanErr)
	writeLogLine(ctx, conn, notice)
	conn.Close(websocket.StatusInternalError, "log stream broken")
	return true
}

// viewerGone reports whether err only says the viewer left. The streams'
// context is the WebSocket's (CloseRead), which ends when the browser closes
// it — leaving a logs tab does — and every read and write bound to it then
// fails with context.Canceled. That is the normal end of a stream, not an
// error worth an ERROR line.
func viewerGone(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}
