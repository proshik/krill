package server

import (
	"context"
	"net/http"
)

// streams returns the context that StopStreams cancels, creating it on first
// use so a Server built as a struct literal (tests) works too.
func (s *Server) streams() context.Context {
	s.streamsOnce.Do(func() {
		s.streamsCtx, s.streamsStop = context.WithCancel(context.Background())
	})
	return s.streamsCtx
}

// StopStreams ends every long-lived streaming response. Register it with
// http.Server.RegisterOnShutdown: Shutdown waits for every active connection,
// and a stream that only ends when its client goes away would otherwise hold
// the shutdown until its deadline. WebSockets are not affected — their
// connections are hijacked, and Shutdown neither tracks nor waits for those.
func (s *Server) StopStreams() {
	s.streams()
	s.streamsStop()
}

// endStreamOnShutdown cancels a GET request's context once StopStreams runs.
// Mounted on /mcp: in streamable HTTP a GET is the session's standalone
// notification stream, which the SDK keeps open until the request context ends
// or the session closes — an MCP client that stays connected (an agent left
// running) would otherwise keep Shutdown waiting for as long as it stays.
// POST (a tool call) and DELETE are short and left to finish within the
// shutdown budget.
func (s *Server) endStreamOnShutdown(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(s.streams(), cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
