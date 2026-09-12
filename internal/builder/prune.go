package builder

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Spellings of the "keep at least this much cache" flag of `docker builder
// prune`. Docker CLI 28+ (buildx) renamed --keep-storage to --reserved-space
// and prints a deprecation warning for the old name; an older CLI, or one
// without the buildx plugin, knows only --keep-storage.
const (
	pruneFlagReserved = "--reserved-space"
	pruneFlagLegacy   = "--keep-storage"
)

// pruneRunner runs `docker <args...>` and returns its combined output.
type pruneRunner func(ctx context.Context, args ...string) ([]byte, error)

// cachePruner remembers which flag spelling the local docker CLI accepts, so a
// CLI that rejects the new one costs one failed attempt, not one per interval.
type cachePruner struct {
	run    pruneRunner
	legacy bool
}

// prune trims the build cache down to keep. It tries --reserved-space first and
// falls back to --keep-storage only when the CLI does not know the new flag.
func (p *cachePruner) prune(ctx context.Context, keep string) error {
	if !p.legacy {
		out, err := p.run(ctx, "builder", "prune", "--force", pruneFlagReserved, keep)
		if err == nil {
			return nil
		}
		if !strings.Contains(string(out), "unknown flag: "+pruneFlagReserved) {
			slog.Warn("build cache prune failed", "err", err, "output", string(out))
			return err
		}
		slog.Info("docker CLI does not know " + pruneFlagReserved + ", pruning the build cache with " + pruneFlagLegacy)
		p.legacy = true
	}
	out, err := p.run(ctx, "builder", "prune", "--force", pruneFlagLegacy, keep)
	if err != nil {
		slog.Warn("build cache prune failed", "err", err, "output", string(out))
	}
	return err
}

// StartCachePrune trims the BuildKit cache on an interval. Nothing else ever
// removes it: every Dockerfile build leaves layers behind, and on a small VPS
// the disk fills silently until builds start failing.
func StartCachePrune(ctx context.Context, dockerHost string, interval time.Duration, keepStorage string) {
	if interval <= 0 || keepStorage == "" {
		return
	}
	p := &cachePruner{run: func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		if dockerHost != "" {
			cmd.Env = append(os.Environ(), "DOCKER_HOST="+dockerHost)
		}
		return cmd.CombinedOutput()
	}}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = p.prune(ctx, keepStorage)
			}
		}
	}()
}
