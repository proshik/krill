package deploy

import (
	"context"
	"log/slog"
	"time"
)

// LogCleaner — what is needed to clean up old logs.
type LogCleaner interface {
	ClearOldDeploymentLogs(ctx context.Context) error
}

// StartLogCleanup clears the logs of deployments older than an hour every interval.
// Returns a stop function.
func StartLogCleanup(ctx context.Context, c LogCleaner, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.ClearOldDeploymentLogs(ctx); err != nil {
					slog.Warn("log cleanup failed", "err", err)
				}
			}
		}
	}()
	return cancel
}
