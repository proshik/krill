package server

import (
	"bufio"
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())

	rc, err := s.engine.ServiceLogs(ctx, dockerName(c.App.ID), true)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "logs unavailable")
		return
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		writeErr := conn.Write(wctx, websocket.MessageText, line)
		cancel()
		if writeErr != nil {
			return
		}
	}
	conn.Close(websocket.StatusNormalClosure, "")
}
