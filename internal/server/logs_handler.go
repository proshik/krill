package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Info("appLogs: websocket accept failed", "err", err, "app_id", c.App.ID)
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())

	rc, err := s.engine.ServiceLogs(ctx, dockerName(c.App.ID), true)
	if err != nil {
		logFrom(r).Error("appLogs: service logs unavailable", "err", err, "app_id", c.App.ID)
		conn.Close(websocket.StatusInternalError, "logs unavailable")
		return
	}
	defer rc.Close()

	streamReaderToWS(ctx, conn, rc)
}

// streamReaderToWS reads lines from rc and sends them to the WS as text frames until the reader
// is exhausted or a write fails. On reader completion it closes the connection normally.
func streamReaderToWS(ctx context.Context, conn *websocket.Conn, rc io.Reader) {
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := conn.Write(wctx, websocket.MessageText, line)
		cancel()
		if err != nil {
			return
		}
	}
	conn.Close(websocket.StatusNormalClosure, "")
}
