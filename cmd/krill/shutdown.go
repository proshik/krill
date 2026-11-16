package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/proshik/krill/internal/server"
)

// shutdownBudget bounds the graceful part of a stop. systemd's default
// TimeoutStopSec (90s, which krill.service does not override) is far above it,
// so systemd never has to SIGKILL a stop that runs into it.
const shutdownBudget = 10 * time.Second

// gracefulShutdown stops environment clone work (stopWork) and the HTTP server within budget.
// A stop that runs out of time is not a failure of the process: whatever is
// left is closed forcibly and logged, and the caller exits normally. Exiting
// non-zero here would make systemd mark every restart — a self-update's
// included — as "Failed", while the restart itself happens either way.
func gracefulShutdown(srv *http.Server, budget time.Duration, inflight *server.Inflight, stopWork func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := stopWork(ctx); err != nil {
		slog.Warn("environment clone work did not stop within the shutdown budget", "budget", budget, "err", err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("graceful shutdown did not finish in time; closing the remaining connections",
			"budget", budget, "err", err, "pending", inflight.Pending())
		if err := srv.Close(); err != nil {
			slog.Warn("closing the HTTP server failed", "err", err)
		}
		return
	}
	slog.Info("shutdown complete")
}
