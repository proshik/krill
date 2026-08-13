package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/coder/websocket"

	"github.com/proshik/krill/internal/logparse"
)

// scanEndNotice describes why a log scan stopped, for the viewer. A clean end
// of stream needs no notice; anything else does — a silent stop is worse than
// an error, because the panel keeps looking live while showing nothing.
func scanEndNotice(err error) (logparse.LogLine, bool) {
	if err == nil {
		return logparse.LogLine{}, false
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return logparse.LogLine{Level: "error", Msg: "log stream stopped: a single log line exceeded the 1 MiB limit"}, true
	}
	return logparse.LogLine{Level: "error", Msg: "log stream stopped: " + err.Error()}, true
}

// streamParsedLogsToWS reads lines from rc, parses each into a LogLine, and
// sends it as a JSON text frame until the reader is exhausted or a write fails.
func streamParsedLogsToWS(ctx context.Context, conn *websocket.Conn, rc io.Reader) {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		b, err := json.Marshal(logparse.ParseLogLine(sc.Text()))
		if err != nil {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		werr := conn.Write(wctx, websocket.MessageText, b)
		cancel()
		if werr != nil {
			return
		}
	}
	if notice, ok := scanEndNotice(sc.Err()); ok {
		if b, err := json.Marshal(notice); err == nil {
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = conn.Write(wctx, websocket.MessageText, b)
			cancel()
		}
		conn.Close(websocket.StatusInternalError, "log stream broken")
		return
	}
	conn.Close(websocket.StatusNormalClosure, "")
}
