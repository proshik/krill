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
	s.serveServiceLogs(w, r, dockerName(c.App.ID), "app_id", c.App.ID)
}

// serveServiceLogs upgrades the request to a WebSocket and streams service's
// runtime log over it (streamServiceLogsToWS) until the stream ends or the
// viewer leaves; attrs identify the resource in log lines.
func (s *Server) serveServiceLogs(w http.ResponseWriter, r *http.Request, service string, attrs ...any) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logFrom(r).Info("service logs: websocket accept failed", append([]any{"err", err}, attrs...)...)
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(context.Background())
	if err := s.streamServiceLogsToWS(ctx, conn, service, i18n.T(r.Context(), "log.gap")); err != nil {
		if viewerGone(ctx, err) {
			return
		}
		logFrom(r).Error("service logs unavailable", append([]any{"err", err}, attrs...)...)
		conn.Close(websocket.StatusInternalError, "logs unavailable")
	}
}
