package server

import (
	"bufio"
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/proshik/krill/internal/docker"
)

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"}, // локальная разработка; на проде сузить
	})
	if err != nil {
		return
	}
	defer c.CloseNow()

	// Обрабатываем входящие control-фреймы (ping/close), пока сами только пишем.
	ctx := c.CloseRead(context.Background())

	rc, err := s.engine.ServiceLogs(ctx, docker.ServiceName(app.Name), true)
	if err != nil {
		c.Close(websocket.StatusInternalError, "logs unavailable")
		return
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		writeErr := c.Write(wctx, websocket.MessageText, line)
		cancel()
		if writeErr != nil {
			return
		}
	}
	c.Close(websocket.StatusNormalClosure, "")
}
