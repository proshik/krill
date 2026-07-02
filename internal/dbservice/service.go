package dbservice

import (
	"context"
	"log/slog"
	"time"

	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
)

// Store — what Service needs from the store.
type Store interface {
	GetInstance(ctx context.Context, id int64) (Instance, error)
	SetInstanceStatus(ctx context.Context, id int64, status string) error
	DeleteInstanceRow(ctx context.Context, id int64) error
}

// Service — DB lifecycle on top of Engine + Store, with a live log via DeployLogHub.
type Service struct {
	engine  docker.Engine
	store   Store
	hub     *deploy.DeployLogHub
	network string
}

func New(engine docker.Engine, store Store, hub *deploy.DeployLogHub, network string) *Service {
	return &Service{engine: engine, store: store, hub: hub, network: network}
}

// dbDeployTimeout bounds a detached DB deploy so a stalled ImagePull (registry
// blackhole) can't leak the goroutine and keep the log feed open forever.
const dbDeployTimeout = 10 * time.Minute

// removeVolume deletes the named volume, retrying briefly. Right after
// ServiceRemove the volume is usually still "in use" while Swarm tears down the
// task container, so a single immediate attempt fails and the volume would leak.
func (s *Service) removeVolume(ctx context.Context, name string) {
	var err error
	for i := 0; i < 12; i++ {
		if err = s.engine.VolumeRemove(ctx, name); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			slog.Error("volume remove", "vol", name, "err", ctx.Err())
			return
		case <-time.After(time.Second):
		}
	}
	slog.Error("volume remove gave up (still in use)", "vol", name, "err", err)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
