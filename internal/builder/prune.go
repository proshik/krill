package builder

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// StartCachePrune trims the BuildKit cache on an interval. Nothing else ever
// removes it: every Dockerfile build leaves layers behind, and on a small VPS
// the disk fills silently until builds start failing.
func StartCachePrune(ctx context.Context, dockerHost string, interval time.Duration, keepStorage string) {
	if interval <= 0 || keepStorage == "" {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cmd := exec.CommandContext(ctx, "docker", "builder", "prune", "--force", "--keep-storage", keepStorage)
				if dockerHost != "" {
					cmd.Env = append(os.Environ(), "DOCKER_HOST="+dockerHost)
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					slog.Warn("build cache prune failed", "err", err, "output", string(out))
				}
			}
		}
	}()
}
