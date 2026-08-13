package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/coder/websocket"

	"github.com/proshik/krill/internal/logparse"
)

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
	if notice, ok := logparse.ScanEndNotice(sc.Err()); ok {
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
