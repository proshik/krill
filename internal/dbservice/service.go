package dbservice

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
)

// Store — what Service needs from the store.
type Store interface {
	GetPostgres(ctx context.Context, id int64) (PostgresDB, error)
	GetRedis(ctx context.Context, id int64) (RedisDB, error)
	SetPostgresStatus(ctx context.Context, id int64, status string) error
	SetRedisStatus(ctx context.Context, id int64, status string) error
	DeletePostgresRow(ctx context.Context, id int64) error
	DeleteRedisRow(ctx context.Context, id int64) error
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

// deployFeedID — separate id space for the DB log hub (negative, so it does not collide with application deployment ids).
func pgFeedID(id int64) int64    { return -(id*2 + 1) }
func redisFeedID(id int64) int64 { return -(id*2 + 2) }

// DeployPostgres: pull → deploy, in a goroutine; logs to hub under pgFeedID(id).
// Runs detached (its own context.Background) on purpose: the deploy must outlive
// the HTTP request that triggered it, which returns immediately with a redirect.
func (s *Service) DeployPostgres(id int64) {
	go s.deployPG(context.Background(), id)
}

func (s *Service) deployPG(ctx context.Context, id int64) {
	pg, err := s.store.GetPostgres(ctx, id)
	if err != nil {
		slog.Error("get pg", "err", err)
		return
	}
	feed := pgFeedID(id)
	var out interface{ Write([]byte) (int, error) } = nopWriter{}
	if s.hub != nil {
		s.hub.Open(feed)
		out = s.hub.Writer(feed)
		defer s.hub.Close(feed)
	}
	fmt.Fprintf(out, "→ pull %s\n", pg.Image)
	if err := s.engine.ImagePull(ctx, pg.Image, out); err != nil {
		fmt.Fprintf(out, "❌ pull failed: %v\n", err)
		_ = s.store.SetPostgresStatus(ctx, id, "error")
		return
	}
	fmt.Fprintf(out, "→ deploy %s\n", pg.AppName)
	if err := s.engine.ServiceDeploy(ctx, postgresSpec(pg, s.network)); err != nil {
		fmt.Fprintf(out, "❌ deploy failed: %v\n", err)
		_ = s.store.SetPostgresStatus(ctx, id, "error")
		return
	}
	fmt.Fprintf(out, "✅ deployed %s\n", pg.AppName)
	_ = s.store.SetPostgresStatus(ctx, id, "running")
}

func (s *Service) StartPostgres(ctx context.Context, id int64) error {
	pg, err := s.store.GetPostgres(ctx, id)
	if err != nil {
		return err
	}
	if err := s.engine.ServiceScale(ctx, pg.AppName, 1); err != nil {
		return err
	}
	return s.store.SetPostgresStatus(ctx, id, "running")
}

func (s *Service) StopPostgres(ctx context.Context, id int64) error {
	pg, err := s.store.GetPostgres(ctx, id)
	if err != nil {
		return err
	}
	if err := s.engine.ServiceScale(ctx, pg.AppName, 0); err != nil {
		return err
	}
	return s.store.SetPostgresStatus(ctx, id, "idle")
}

func (s *Service) DeletePostgres(ctx context.Context, id int64, destroyData bool) error {
	pg, err := s.store.GetPostgres(ctx, id)
	if err != nil {
		return err
	}
	_ = s.engine.ServiceRemove(ctx, pg.AppName) // volume is preserved by default
	if destroyData {
		if err := s.engine.VolumeRemove(ctx, volumeName(pg.AppName)); err != nil {
			slog.Error("volume remove", "vol", volumeName(pg.AppName), "err", err)
		}
	}
	return s.store.DeletePostgresRow(ctx, id)
}

// --- Redis (mirrored) ---
// DeployRedis runs detached on purpose (see DeployPostgres): the deploy must
// outlive the triggering HTTP request, which returns immediately with a redirect.
func (s *Service) DeployRedis(id int64) { go s.deployRedis(context.Background(), id) }

func (s *Service) deployRedis(ctx context.Context, id int64) {
	r, err := s.store.GetRedis(ctx, id)
	if err != nil {
		slog.Error("get redis", "err", err)
		return
	}
	feed := redisFeedID(id)
	var out interface{ Write([]byte) (int, error) } = nopWriter{}
	if s.hub != nil {
		s.hub.Open(feed)
		out = s.hub.Writer(feed)
		defer s.hub.Close(feed)
	}
	fmt.Fprintf(out, "→ pull %s\n", r.Image)
	if err := s.engine.ImagePull(ctx, r.Image, out); err != nil {
		fmt.Fprintf(out, "❌ pull failed: %v\n", err)
		_ = s.store.SetRedisStatus(ctx, id, "error")
		return
	}
	fmt.Fprintf(out, "→ deploy %s\n", r.AppName)
	if err := s.engine.ServiceDeploy(ctx, redisSpec(r, s.network)); err != nil {
		fmt.Fprintf(out, "❌ deploy failed: %v\n", err)
		_ = s.store.SetRedisStatus(ctx, id, "error")
		return
	}
	fmt.Fprintf(out, "✅ deployed %s\n", r.AppName)
	_ = s.store.SetRedisStatus(ctx, id, "running")
}

func (s *Service) StartRedis(ctx context.Context, id int64) error {
	r, err := s.store.GetRedis(ctx, id)
	if err != nil {
		return err
	}
	if err := s.engine.ServiceScale(ctx, r.AppName, 1); err != nil {
		return err
	}
	return s.store.SetRedisStatus(ctx, id, "running")
}

func (s *Service) StopRedis(ctx context.Context, id int64) error {
	r, err := s.store.GetRedis(ctx, id)
	if err != nil {
		return err
	}
	if err := s.engine.ServiceScale(ctx, r.AppName, 0); err != nil {
		return err
	}
	return s.store.SetRedisStatus(ctx, id, "idle")
}

func (s *Service) DeleteRedis(ctx context.Context, id int64, destroyData bool) error {
	r, err := s.store.GetRedis(ctx, id)
	if err != nil {
		return err
	}
	_ = s.engine.ServiceRemove(ctx, r.AppName)
	if destroyData {
		if err := s.engine.VolumeRemove(ctx, volumeName(r.AppName)); err != nil {
			slog.Error("volume remove", "vol", volumeName(r.AppName), "err", err)
		}
	}
	return s.store.DeleteRedisRow(ctx, id)
}

// PgFeedID/RedisFeedID — exported for the deploy log WS handler.
func PgFeedID(id int64) int64    { return pgFeedID(id) }
func RedisFeedID(id int64) int64 { return redisFeedID(id) }

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
