package server

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/web/i18n"
)

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	release, ok := s.acquireLogSlotFor(auth.UserID(r.Context()))
	if !ok {
		http.Error(w, "too many live log streams, try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer release()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Info("appLogs: websocket accept failed", "err", err, "app_id", c.App.ID)
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())

	if err := s.streamServiceLogsToWS(ctx, conn, dockerName(c.App.ID), i18n.T(r.Context(), "log.gap")); err != nil {
		logFrom(r).Error("appLogs: service logs unavailable", "err", err, "app_id", c.App.ID)
		conn.Close(websocket.StatusInternalError, "logs unavailable")
	}
}
