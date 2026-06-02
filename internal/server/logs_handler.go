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

	streamReaderToWS(ctx, conn, rc)
}

// streamReaderToWS читает строки из rc и шлёт их в WS как text-фреймы, пока reader не иссякнет
// или запись не упадёт. На завершение reader закрывает соединение нормально.
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
