package server

import (
	"context"
	"net/http"

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

	streamParsedLogsToWS(ctx, conn, rc)
}
