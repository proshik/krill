package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/proshik/krill/internal/auth"
)

// requestLogger emits one structured slog line per HTTP request (method, path,
// status, bytes, duration, request id, and the user id when authenticated).
// 5xx responses are logged at error level, everything else at info.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		defer func() {
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
				"remote", r.RemoteAddr,
			}
			if uid := auth.UserID(r.Context()); uid != 0 {
				attrs = append(attrs, "user_id", uid)
			}
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}
			slog.Default().Log(r.Context(), level, "request", attrs...)
		}()
		next.ServeHTTP(ww, r)
	})
}

// logFrom returns a logger pre-tagged with the request's id, user, and org so
// handlers can attach those without repeating the lookups.
func logFrom(r *http.Request) *slog.Logger {
	l := slog.Default().With("request_id", middleware.GetReqID(r.Context()))
	if uid := auth.UserID(r.Context()); uid != 0 {
		l = l.With("user_id", uid)
	}
	if oid := auth.OrgID(r.Context()); oid != 0 {
		l = l.With("org_id", oid)
	}
	return l
}
