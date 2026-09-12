package dbservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
)

// Instance — a DBMS server (org-level): one Swarm service on a chosen node,
// holding N logical databases (postgres) or serving apps directly (redis).
// Canonical shape lives in drivers.Instance; aliased here so existing
// dbservice/server code keeps compiling unchanged.
type Instance = drivers.Instance

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

// instanceSpec dispatches to the instance's engine driver — the per-engine
// env/args/mounts knowledge lives in internal/dbservice/drivers, which must
// not import dbservice, so the instance-wide resource defaults (config-driven,
// not known to a driver) are applied here instead when the driver's spec
// leaves them unset. A method (not a package function) so it can read them.
func (s *Service) instanceSpec(inst Instance, network string) docker.ServiceSpec {
	spec := drivers.Registry.MustGet(inst.Engine).BuildSpec(inst, network)
	if spec.MemoryLimitBytes == 0 {
		spec.MemoryLimitBytes = s.defaultMemBytes
	}
	if spec.NanoCPUs == 0 {
		spec.NanoCPUs = s.defaultNanoCPUs
	}
	return spec
}

// PostgresURL — connection string for a logical DB over the overlay network.
// scheme is "postgresql" or "postgres" (validated by the caller). Delegates to
// the postgres driver's LinkValue "url" field (host=inst.AppName, port=5432 —
// an internal/in-cluster connection, same shape as an app-link injection).
func PostgresURL(scheme string, inst Instance, ldb LogicalDB) string {
	v, _ := drivers.LinkValue("postgres", drivers.LinkSource{
		AppName: inst.AppName, Superuser: ldb.Username, Password: ldb.Password, DBName: ldb.DBName, Scheme: scheme,
	}, "url")
	return v
}

// PostgresExternalURL — connection string for a logical DB over the
// control-plane's public host/external_port (via the socat proxy).
func PostgresExternalURL(inst Instance, ldb LogicalDB, host string) string {
	p := ""
	if inst.ExternalPort != nil {
		p = strconv.Itoa(int(*inst.ExternalPort))
	}
	return drivers.URLString("postgresql", ldb.Username, ldb.Password, host, p, ldb.DBName)
}

// RedisInternalURL/RedisExternalURL — connection string builders for a Redis
// instance (internal overlay DNS / external host port).
func RedisInternalURL(inst Instance) string {
	v, _ := drivers.LinkValue("redis", drivers.LinkSource{AppName: inst.AppName, Password: inst.SuperuserPassword, Scheme: "redis"}, "url")
	return v
}

func RedisExternalURL(inst Instance, host string) string {
	p := ""
	if inst.ExternalPort != nil {
		p = strconv.Itoa(int(*inst.ExternalPort))
	}
	return drivers.URLString("redis", "default", inst.SuperuserPassword, host, p, "")
}

// DeployInstance: pull → deploy, in a goroutine, detached from the triggering
// HTTP request (which returns immediately with a redirect) so the deploy
// outlives it.
func (s *Service) DeployInstance(id int64) {
	// One deploy per instance at a time: a double-clicked Deploy button would
	// otherwise race two ServiceDeploy calls and two status writes, and the
	// loser's outcome would overwrite the winner's.
	lock := oplock.DBInstanceDeploy(id)
	if !oplock.TryAcquire(lock) {
		slog.Warn("db instance deploy already in progress, ignoring duplicate trigger", "instance_id", id)
		return
	}
	go func() {
		defer oplock.Release(lock)
		ctx, cancel := context.WithTimeout(context.Background(), dbDeployTimeout)
		defer cancel()
		s.deployInstance(ctx, id)
	}()
}

func (s *Service) deployInstance(ctx context.Context, id int64) {
	feed := InstanceFeedID(id)
	var out interface{ Write([]byte) (int, error) } = nopWriter{}
	if s.hub != nil {
		s.hub.Open(feed)
		out = s.hub.Writer(feed)
		defer s.hub.Close(feed)
	}
	_ = s.deployCore(ctx, id, out)
}

// deployCore pulls and deploys the instance's service writing progress to out,
// setting status running/error. Extracted from deployInstance so the migration
// job can redeploy inside its own log feed.
func (s *Service) deployCore(ctx context.Context, id int64, out io.Writer) error {
	// Terminal status writes must outlive ctx: a deploy that trips its own
	// timeout (or is cancelled by shutdown) would otherwise fail to persist the
	// outcome, leaving the instance showing its previous status forever.
	stCtx := context.WithoutCancel(ctx)

	inst, err := s.store.GetInstance(ctx, id)
	if err != nil {
		slog.Error("get db instance", "err", err)
		return err
	}
	fmt.Fprintf(out, "→ pull %s\n", inst.Image)
	if err := s.engine.ImagePull(ctx, inst.Image, out); err != nil {
		fmt.Fprintf(out, "❌ pull failed: %v\n", err)
		slog.Error("db instance deploy: image pull failed", "err", err, "instance_id", id, "image", inst.Image)
		_ = s.store.SetInstanceStatus(stCtx, id, "error")
		return err
	}
	netName, err := s.networkFor(ctx, inst)
	if err != nil {
		fmt.Fprintf(out, "❌ resolve organization network failed: %v\n", err)
		slog.Error("db instance deploy: resolve organization network failed", "err", err, "instance_id", id)
		_ = s.store.SetInstanceStatus(stCtx, id, "error")
		return err
	}
	fmt.Fprintf(out, "→ deploy %s\n", inst.AppName)
	if err := s.engine.ServiceDeploy(ctx, s.instanceSpec(inst, netName)); err != nil {
		fmt.Fprintf(out, "❌ deploy failed: %v\n", err)
		slog.Error("db instance deploy: service deploy failed", "err", err, "instance_id", id, "app_name", inst.AppName, "node", inst.NodeHostname)
		_ = s.store.SetInstanceStatus(stCtx, id, "error")
		return err
	}
	fmt.Fprintf(out, "✅ deployed %s\n", inst.AppName)
	_ = s.store.SetInstanceStatus(stCtx, id, "running")
	if err := s.reconcileProxy(ctx, inst); err != nil {
		fmt.Fprintf(out, "⚠ external-access proxy: %v\n", err)
		slog.Warn("db instance: reconcile proxy failed", "err", err, "instance_id", id)
	}
	return nil
}

// ParkInstance rewrites a database instance's Swarm service from its stored
// configuration onto its organization network at the given replica count, and
// moves its external-access proxy along with it. Unlike DeployInstance it runs
// synchronously, pulls nothing and leaves the recorded status alone.
//
// It exists for the startup network migration, which must move a stopped
// instance WITHOUT starting it: StartInstance only scales the existing service,
// so a stopped instance simply skipped by the migration would come back up on
// the old shared network the next time someone pressed Start.
func (s *Service) ParkInstance(ctx context.Context, id int64, replicas uint64) error {
	lock := oplock.DBInstanceDeploy(id)
	if !oplock.TryAcquire(lock) {
		return errors.New("a deploy of this instance is already in progress")
	}
	defer oplock.Release(lock)

	inst, err := s.store.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	netName, err := s.networkFor(ctx, inst)
	if err != nil {
		return fmt.Errorf("resolve organization network: %w", err)
	}
	spec := s.instanceSpec(inst, netName)
	spec.Replicas = replicas
	if err := s.engine.ServiceDeploy(ctx, spec); err != nil {
		return fmt.Errorf("deploy service: %w", err)
	}
	if err := s.reconcileProxy(ctx, inst); err != nil {
		return fmt.Errorf("external-access proxy: %w", err)
	}
	return nil
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
	// Remove every proxy the instance's driver declares (postgres/redis: one;
	// a future N-target driver like minio: one per published port, e.g. data +
	// console) — not-found is tolerated per target (proxy was never deployed
	// for that target, or already removed).
	for _, t := range drivers.Registry.MustGet(inst.Engine).ExternalTargets(inst) {
		name := proxyName(id) + t.Suffix
		if err := s.engine.ServiceRemove(rmCtx, name); err != nil && !isNotFound(err) {
			slog.Error("delete db instance: proxy remove failed", "err", err, "instance_id", id, "proxy", name)
			return fmt.Errorf("remove proxy %s: %w", name, err)
		}
	}
	if destroyData {
		vol := volumeName(inst.AppName)
		nodeID, ok := "", false
		if nodes, nerr := s.engine.Nodes(rmCtx); nerr == nil {
			nodeID, ok = resolveNodeID(nodes, inst.NodeHostname)
		}
		if ok {
			if err := s.removeVolumeOn(rmCtx, vol, nodeID); err != nil {
				slog.Error("delete db instance: volume remove failed", "err", err, "instance_id", id, "vol", vol, "node", inst.NodeHostname)
			}
		} else {
			// Node lookup failed (single-node deployment with no cluster_nodes,
			// or a transient Nodes() error) — fall back to the old local-only
			// removal so a plain single-node install keeps working unchanged.
			s.removeVolume(rmCtx, vol)
		}
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
