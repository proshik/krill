package dbservice

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/proshik/krill/internal/docker"
)

// Instance — a DBMS server (org-level): one Swarm service on a chosen node,
// holding N logical databases (postgres) or serving apps directly (redis).
type Instance struct {
	ID                int64
	OrganizationID    int64
	Engine            string // "postgres" | "redis"
	Name              string
	AppName           string // Swarm service name = overlay DNS host = volume prefix
	Image             string
	Superuser         string // postgres only; "" for redis
	SuperuserPassword string // redis: the requirepass value
	ExternalPort      *int32
	Status            string
	NodeHostname      string // "" = control-plane (manager)
}

// LogicalDB — a database inside a postgres Instance, owned by an environment.
type LogicalDB struct {
	ID            int64
	InstanceID    int64
	EnvironmentID int64
	Name          string
	DBName        string
	Username      string
	Password      string
}

// InstanceFeedID — the DB log-hub feed for an instance (negative, single id
// space: instances live in one table, unlike the legacy pg/redis pair).
func InstanceFeedID(id int64) int64 { return -id }

func instanceSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:        inst.AppName,
		Image:       inst.Image,
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{dbConstraint(inst.NodeHostname)},
	}
	switch inst.Engine {
	case "postgres":
		// No POSTGRES_DB: it only matters on first volume init; databases are
		// created by provisioning (converted instances have an initialized volume).
		spec.Env = map[string]string{
			"POSTGRES_USER":     inst.Superuser,
			"POSTGRES_PASSWORD": inst.SuperuserPassword,
		}
		spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: "/var/lib/postgresql/data"}}
	case "redis":
		// Exec form (no shell): the password is a discrete argv element.
		spec.Args = []string{"redis-server", "--requirepass", inst.SuperuserPassword}
		spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: "/data"}}
	}
	return spec
}

// PostgresURL — connection string for a logical DB over the overlay network.
// scheme is "postgresql" or "postgres" (validated by the caller).
func PostgresURL(scheme string, inst Instance, ldb LogicalDB) string {
	return scheme + "://" + ldb.Username + ":" + ldb.Password + "@" + inst.AppName + ":5432/" + ldb.DBName
}

func PostgresExternalURL(inst Instance, ldb LogicalDB, host string) string {
	p := ""
	if inst.ExternalPort != nil {
		p = strconv.Itoa(int(*inst.ExternalPort))
	}
	return "postgresql://" + ldb.Username + ":" + ldb.Password + "@" + host + ":" + p + "/" + ldb.DBName
}

// RedisInternalURL/RedisExternalURL — connection string builders for a Redis
// instance (internal overlay DNS / external host port).
func RedisInternalURL(inst Instance) string {
	return "redis://default:" + inst.SuperuserPassword + "@" + inst.AppName + ":6379"
}

func RedisExternalURL(inst Instance, host string) string {
	p := ""
	if inst.ExternalPort != nil {
		p = strconv.Itoa(int(*inst.ExternalPort))
	}
	return "redis://default:" + inst.SuperuserPassword + "@" + host + ":" + p
}

// DeployInstance: pull → deploy, in a goroutine, detached from the triggering
// HTTP request (which returns immediately with a redirect) so the deploy
// outlives it.
func (s *Service) DeployInstance(id int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbDeployTimeout)
		defer cancel()
		s.deployInstance(ctx, id)
	}()
}

func (s *Service) deployInstance(ctx context.Context, id int64) {
	inst, err := s.store.GetInstance(ctx, id)
	if err != nil {
		slog.Error("get db instance", "err", err)
		return
	}
	feed := InstanceFeedID(id)
	var out interface{ Write([]byte) (int, error) } = nopWriter{}
	if s.hub != nil {
		s.hub.Open(feed)
		out = s.hub.Writer(feed)
		defer s.hub.Close(feed)
	}
	fmt.Fprintf(out, "→ pull %s\n", inst.Image)
	if err := s.engine.ImagePull(ctx, inst.Image, out); err != nil {
		fmt.Fprintf(out, "❌ pull failed: %v\n", err)
		slog.Error("db instance deploy: image pull failed", "err", err, "instance_id", id, "image", inst.Image)
		_ = s.store.SetInstanceStatus(ctx, id, "error")
		return
	}
	fmt.Fprintf(out, "→ deploy %s\n", inst.AppName)
	if err := s.engine.ServiceDeploy(ctx, instanceSpec(inst, s.network)); err != nil {
		fmt.Fprintf(out, "❌ deploy failed: %v\n", err)
		slog.Error("db instance deploy: service deploy failed", "err", err, "instance_id", id, "app_name", inst.AppName, "node", inst.NodeHostname)
		_ = s.store.SetInstanceStatus(ctx, id, "error")
		return
	}
	fmt.Fprintf(out, "✅ deployed %s\n", inst.AppName)
	_ = s.store.SetInstanceStatus(ctx, id, "running")
	if err := s.reconcileProxy(ctx, inst); err != nil {
		fmt.Fprintf(out, "⚠ external-access proxy: %v\n", err)
		slog.Warn("db instance: reconcile proxy failed", "err", err, "instance_id", id)
	}
}

func (s *Service) StartInstance(ctx context.Context, id int64) error {
	inst, err := s.store.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	if err := s.engine.ServiceScale(ctx, inst.AppName, 1); err != nil {
		return err
	}
	return s.store.SetInstanceStatus(ctx, id, "running")
}

func (s *Service) StopInstance(ctx context.Context, id int64) error {
	inst, err := s.store.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	if err := s.engine.ServiceScale(ctx, inst.AppName, 0); err != nil {
		return err
	}
	return s.store.SetInstanceStatus(ctx, id, "idle")
}

// RemoveInstanceContainers removes the instance's Swarm service and its
// control-plane proxy (engine-gated: nil engine → no-op) and, if requested,
// the data volume — WITHOUT touching the db_instances row. Split out from
// DeleteInstance so callers that must first clear other rows referencing the
// instance (logical_databases has an ON DELETE RESTRICT FK to db_instances)
// can sequence "containers are actually gone" before "bookkeeping rows are
// gone": a real docker failure here must not silently destroy backups/links
// for an instance that is still running.
func (s *Service) RemoveInstanceContainers(ctx context.Context, id int64, destroyData bool) error {
	inst, err := s.store.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	if s.engine == nil {
		return nil
	}
	// Detach from the request ctx so a cancelled request cannot leave a
	// half-removed instance; tolerate not-found (already gone) but surface a
	// real failure so the row+credentials are kept and the delete is retryable.
	rmCtx := context.WithoutCancel(ctx)
	if err := s.engine.ServiceRemove(rmCtx, inst.AppName); err != nil && !isNotFound(err) {
		slog.Error("delete db instance: service remove failed", "err", err, "instance_id", id, "app_name", inst.AppName)
		return fmt.Errorf("remove service: %w", err)
	}
	if err := s.engine.ServiceRemove(rmCtx, proxyName(id)); err != nil && !isNotFound(err) {
		slog.Error("delete db instance: proxy remove failed", "err", err, "instance_id", id)
		return fmt.Errorf("remove proxy: %w", err)
	}
	if destroyData {
		s.removeVolume(rmCtx, volumeName(inst.AppName))
	}
	return nil
}

// DeleteInstanceRow deletes the db_instances row directly, without touching
// docker. Used by callers (e.g. the deleteDBInstance handler) that already
// removed the containers via RemoveInstanceContainers and cleared any rows
// with a RESTRICT FK to db_instances (logical_databases) first.
func (s *Service) DeleteInstanceRow(ctx context.Context, id int64) error {
	return s.store.DeleteInstanceRow(ctx, id)
}

// DeleteInstance removes the Swarm service (engine-gated: nil engine → row-only
// delete, tests only) and optionally the data volume, then the row. A real
// ServiceRemove failure is surfaced (not swallowed): the row is kept so the
// delete is retryable; a not-found failure is tolerated (already gone).
func (s *Service) DeleteInstance(ctx context.Context, id int64, destroyData bool) error {
	if err := s.RemoveInstanceContainers(ctx, id, destroyData); err != nil {
		return err
	}
	return s.store.DeleteInstanceRow(ctx, id)
}
