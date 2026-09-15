package main

import (
	"context"
	"log/slog"
)

// updateBusy tells the self-updater whether restarting Krill now would cut
// short work in flight. The first busy source wins; a failed lookup counts as
// busy (fail closed) and is logged.
type updateBusy struct {
	runningDeploys     func(context.Context) (int64, error)
	migratingInstances func(context.Context) (int64, error)
	backups            interface{ InFlight() int }
	volumes            interface{ InFlight() int }
}

// reason reports why restarting Krill right now would be disruptive, as one
// of the machine codes the Updates page knows how to explain ("deploy",
// "backup", "volume_backup", "db_migration"), or "" when nothing is in
// flight.
func (b updateBusy) reason(ctx context.Context) string {
	if n, err := b.runningDeploys(ctx); err != nil {
		slog.Warn("self-update: could not check running deploys; treating as busy", "err", err)
		return "deploy"
	} else if n > 0 {
		return "deploy"
	}
	if b.backups.InFlight() > 0 {
		return "backup"
	}
	if b.volumes.InFlight() > 0 {
		return "volume_backup"
	}
	if n, err := b.migratingInstances(ctx); err != nil {
		slog.Warn("self-update: could not check migrating DB instances; treating as busy", "err", err)
		return "db_migration"
	} else if n > 0 {
		return "db_migration"
	}
	return ""
}
