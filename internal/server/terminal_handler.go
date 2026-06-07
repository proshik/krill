package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/proshik/krill/internal/docker"
)

// maxTermDimension caps resize values to reject absurd/garbage sizes.
const maxTermDimension = 1000

// parseResize parses a {"rows":R,"cols":C} control frame. ok=false on bad JSON
// or out-of-range values.
func parseResize(data []byte) (rows, cols uint, ok bool) {
	var m struct {
		Rows uint `json:"rows"`
		Cols uint `json:"cols"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, 0, false
	}
	if m.Rows == 0 || m.Cols == 0 || m.Rows > maxTermDimension || m.Cols > maxTermDimension {
		return 0, 0, false
	}
	return m.Rows, m.Cols, true
}

// wsConn is the slice of *websocket.Conn that pumpTerminal needs (so it can be
// faked in tests). *websocket.Conn satisfies it directly.
type wsConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, data []byte) error
	Close(code websocket.StatusCode, reason string) error
}

// pumpTerminal bridges a WebSocket and an exec session until either side ends:
// binary frames are stdin, text frames are resize control, session output is
// sent back as binary frames. idle>0 closes the session after that long with no
// I/O. The session and WS are each closed exactly once.
func pumpTerminal(ctx context.Context, ws wsConn, sess docker.ExecSession, idle time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	var once sync.Once
	shutdown := func(code websocket.StatusCode, reason string) {
		once.Do(func() {
			cancel()
			_ = sess.Close()
			_ = ws.Close(code, reason)
		})
	}
	defer shutdown(websocket.StatusNormalClosure, "")

	activity := make(chan struct{}, 1)
	bump := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				bump()
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					shutdown(websocket.StatusInternalError, "write failed")
					return
				}
			}
			if err != nil {
				shutdown(websocket.StatusNormalClosure, "session ended")
				return
			}
		}
	}()

	if idle > 0 {
		go func() {
			t := time.NewTimer(idle)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-activity:
					if !t.Stop() {
						<-t.C
					}
					t.Reset(idle)
				case <-t.C:
					shutdown(websocket.StatusPolicyViolation, "idle timeout")
					return
				}
			}
		}()
	}

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			shutdown(websocket.StatusNormalClosure, "")
			return
		}
		bump()
		switch typ {
		case websocket.MessageText:
			if rows, cols, ok := parseResize(data); ok {
				if rerr := sess.Resize(ctx, rows, cols); rerr != nil {
					slog.Warn("terminal: resize failed", "err", rerr)
				}
			}
		case websocket.MessageBinary:
			if _, werr := sess.Write(data); werr != nil {
				shutdown(websocket.StatusInternalError, "stdin failed")
				return
			}
		}
	}
}
