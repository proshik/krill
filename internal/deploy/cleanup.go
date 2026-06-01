package deploy

import (
	"context"
	"log/slog"
	"time"
)

// LogCleaner — то, что нужно для очистки старых логов.
type LogCleaner interface {
	ClearOldDeploymentLogs(ctx context.Context) error
}

// StartLogCleanup раз в interval обнуляет логи деплоев старше часа.
// Возвращает функцию остановки.
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
