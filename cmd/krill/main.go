package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/cluster"
	"github.com/proshik/krill/internal/config"
	"github.com/proshik/krill/internal/database"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/orgnet"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/traefik"
	"github.com/proshik/krill/internal/volume"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// setupLogging installs the default slog handler from config (level + format).
func setupLogging(level, format string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(format) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel, cfg.LogFormat)
	slog.Info("starting krill", "listen", cfg.ListenAddr, "base_domain", cfg.BaseDomain, "network", cfg.Network)

	// Encryption-at-rest for stored credentials (opt-in via KRILL_SECRET_KEY).
	secret.Init(cfg.SecretKey)
	if !secret.Enabled() {
		slog.Warn("KRILL_SECRET_KEY not set — stored secrets (DB passwords, registry/destination credentials) are NOT encrypted at rest")
	}

	// Migrations on startup.
	if err := database.RunMigrations(cfg.DatabaseURL); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	q := db.New(pool)

	// Seed admin.
	authSvc := auth.NewService(q)
	if err := authSvc.SeedAdmin(ctx, cfg.AdminEmail, cfg.AdminPassword); err != nil {
		return err
	}
	orgSvc := org.NewService(q)

	hub := deploy.NewLogHub()
	b := builder.New(cfg.DockerHost, cfg.AllowPrivateEgress)
	if err := builder.Available(); err != nil {
		slog.Warn("build dependencies missing (dockerfile builds disabled)", "err", err)
	}

	engine, err := docker.NewEngine(cfg.DockerHost)
	if err != nil {
		return err
	}
	acme := traefik.AcmeConfig{Email: cfg.AcmeContact(), Staging: cfg.AcmeStaging}
	// The gateway has to sit in every organization's network, not just the base
	// one, or it cannot route to services that live behind the per-organization
	// isolation. Names come from orgnet.Name, not from the stored network_name:
	// on an install that has not been migrated yet the column is still empty,
	// and Traefik has to be in those networks BEFORE the migrator moves any
	// service into them.
	// A failed listing must not reconcile: an empty list is indistinguishable
	// from "this install has no organizations", and applying it would detach the
	// running gateway from every organization network over a transient DB error.
	gatewayReady := false
	if orgNets, nerr := orgNetworks(ctx, q); nerr != nil {
		slog.Warn("could not list organization networks; leaving the gateway as it is", "err", nerr)
	} else if rerr := traefik.Reconcile(ctx, engine, cfg.Network, orgNets, acme); rerr != nil {
		slog.Warn("traefik reconcile failed (continuing)", "err", rerr)
	} else {
		gatewayReady = true
	}

	store := deploy.NewDBStore(q)
	deploy.SetConvergeTimeout(cfg.ConvergeTimeout)

	// Notifications: Telegram alerts on failures + health transitions. Wire the
	// notifier into the deployer BEFORE Start so the worker never reads the field
	// concurrently with this assignment.
	notifyStore := notify.NewDBStore(q)
	notifySvc := notify.New(notifyStore)

	// Deploys interrupted by a crash or a kill -9 are still marked 'running' and
	// nothing will ever finish them. Reconcile before the worker starts: a
	// just-booted process owns no in-flight deploy.
	if n, err := store.FailOrphanedDeployments(ctx); err != nil {
		slog.Error("could not reconcile interrupted deployments", "err", err)
	} else if n > 0 {
		slog.Warn("marked interrupted deployments as failed", "count", n)
	}

	dep := deploy.New(engine, b, store, hub, cfg.Network)
	dep.SetNotifier(notifySvc)

	// Instance-wide default resource limits: an unlimited container can take
	// the whole host and starve every other tenant, so apps/DB instances with
	// no explicit value fall back to these. Empty means "no default" (not an
	// error, ParseMemoryBytes/ParseNanoCPUs already return (0, nil) for it); a
	// non-empty value that fails to parse is a misconfiguration — warn and fall
	// back to no limit rather than aborting startup over it.
	defaultMemBytes, err := docker.ParseMemoryBytes(cfg.DefaultMemoryLimit)
	if err != nil {
		slog.Warn("invalid KRILL_DEFAULT_MEMORY_LIMIT, no default memory limit will be applied", "value", cfg.DefaultMemoryLimit, "err", err)
		defaultMemBytes = 0
	}
	defaultNanoCPUs, err := docker.ParseNanoCPUs(cfg.DefaultCPULimit)
	if err != nil {
		slog.Warn("invalid KRILL_DEFAULT_CPU_LIMIT, no default CPU limit will be applied", "value", cfg.DefaultCPULimit, "err", err)
		defaultNanoCPUs = 0
	}
	dep.SetResourceDefaults(defaultMemBytes, defaultNanoCPUs)
	dep.SetMaxBuildsPerOrg(cfg.MaxBuildsPerOrg)

	dep.Start(ctx)
	defer dep.Stop()

	// Nothing else ever trims the BuildKit cache: every Dockerfile build leaves
	// layers behind, and on a small VPS the disk fills silently until builds
	// start failing.
	builder.StartCachePrune(ctx, cfg.DockerHost, cfg.BuildPruneInterval, cfg.BuildCacheLimit)

	stopCleanup := deploy.StartLogCleanup(ctx, store, 10*time.Minute)
	defer stopCleanup()

	// Session GC: expired rows are otherwise only reaped lazily when the exact
	// token is re-presented (never, once the browser drops the cookie).
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := authSvc.PruneExpiredSessions(ctx); err != nil {
					slog.Warn("session prune failed", "err", err)
				}
			}
		}
	}()

	dbStore := dbservice.NewDBStore(q)
	dbSvc := dbservice.New(engine, dbStore, hub, cfg.Network)
	dbSvc.SetResourceDefaults(defaultMemBytes, defaultNanoCPUs)

	// Boot sweep: a control-plane restart kills any in-process migration job,
	// leaving rows stuck at 'migrating' (the oplock is in-memory, so a fresh
	// process starts with none held) — reset them to 'error' before serving.
	if err := q.ResetMigratingInstances(ctx); err != nil {
		slog.Warn("reset stale migrating instances", "err", err)
	}

	// Backups: service + in-process cron scheduler.
	backupStore := backup.NewDBStore(q)
	backupSvc := backup.New(engine, backupStore, cfg.AllowPrivateEgress)
	backupSvc.SetNotifier(notifySvc)

	dbSvc.SetMigrateTimeout(cfg.MigrateTimeout)
	dbSvc.SetNotifier(notifySvc)

	// Health watcher: polls service state for app down/recovered alerts.
	watcher := notify.NewWatcher(engine, notifyStore, notifySvc, cfg.HealthPollInterval)
	go watcher.Run(ctx)

	// Monitoring: sample container stats into Postgres for the Monitoring page.
	// The control-plane node is labelled "control-plane" (a friendly, stable name
	// matching the rest of the UI, e.g. the managed-DB node picker) rather than its
	// raw Swarm hostname; workers keep their cluster_nodes name (e.g. "worker-1").
	localName := "control-plane"
	// cpuCaches holds one CPUCache per worker node, kept alive across sampler
	// ticks. NewRemoteStats(WithCache) with a fresh cache every tick would make
	// every CPU% sample a "first sample" (always 0) — see docker.NewRemoteStatsWithCache.
	// workerLister runs single-threaded, once per tick, before SampleAll's
	// goroutines fan out, so this map needs no mutex.
	cpuCaches := map[string]*docker.CPUCache{}
	workerLister := func(c context.Context) ([]metrics.Worker, error) {
		rows, err := q.ListClusterNodes(c)
		if err != nil {
			return nil, err
		}
		ws := make([]metrics.Worker, 0, len(rows))
		live := make(map[string]bool, len(rows))
		for _, row := range rows {
			row := row
			key := row.SwarmNodeID
			if key == "" {
				key = row.Name
			}
			live[key] = true
			cache := cpuCaches[key]
			if cache == nil {
				cache = docker.NewCPUCache()
				cpuCaches[key] = cache
			}
			ws = append(ws, metrics.Worker{
				Name: row.Name,
				Connect: func(cc context.Context) (metrics.NodeStatsSource, func() error, error) {
					key, kerr := secret.Dec(row.SshKey)
					if kerr != nil {
						return nil, nil, fmt.Errorf("cluster node %d ssh key: %w", row.ID, kerr)
					}
					cl, derr := cluster.DialVerified(cluster.JoinSpec{
						Host:       row.SshHost,
						Port:       int(row.SshPort),
						User:       row.SshUser,
						PrivateKey: []byte(key),
						HostKey:    row.HostKey,
					}, cfg.MetricsNodeTimeout)
					if derr != nil {
						return nil, nil, derr
					}
					rs, rerr := docker.NewRemoteStatsWithCache(func(_ context.Context, _, _ string) (net.Conn, error) {
						return cl.Dial("unix", "/var/run/docker.sock")
					}, cache)
					if rerr != nil {
						_ = cl.Close()
						return nil, nil, rerr
					}
					return rs, func() error { _ = rs.Close(); return cl.Close() }, nil
				},
			})
		}
		// Drop caches for nodes no longer in the cluster (renamed/removed) so
		// they don't leak forever.
		for k := range cpuCaches {
			if !live[k] {
				delete(cpuCaches, k)
			}
		}
		return ws, nil
	}
	metricsStore := metrics.NewDBStore(q)
	clusterSrc := metrics.NewClusterSource(localName, engine, workerLister, cfg.MetricsNodeTimeout)
	metricsSampler := metrics.NewSampler(clusterSrc, metricsStore, cfg.MetricsInterval, cfg.MetricsRetention)
	go metricsSampler.Run(ctx)

	// Cross-node exec: route provisioning/backup/restore/terminal to the docker
	// daemon of the node a container runs on. cluster_nodes holds only workers
	// (matched by swarm_node_id); a task on any other node is on the control plane
	// → local exec. Reuses the monitoring SSH-tunnel wiring.
	if rc, ok := engine.(docker.RemoteExecConfigurable); ok {
		rc.SetRemoteClientProvider(func(ctx context.Context, nodeID string) (*client.Client, func() error, error) {
			row, err := q.GetClusterNodeBySwarmID(ctx, nodeID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil, nil // control-plane / not a registered worker → local
			}
			if err != nil {
				return nil, nil, err
			}
			key, kerr := secret.Dec(row.SshKey)
			if kerr != nil {
				return nil, nil, fmt.Errorf("cluster node %d ssh key: %w", row.ID, kerr)
			}
			cl, derr := cluster.DialVerified(cluster.JoinSpec{
				Host: row.SshHost, Port: int(row.SshPort), User: row.SshUser,
				PrivateKey: []byte(key), HostKey: row.HostKey,
			}, cfg.MetricsNodeTimeout)
			if derr != nil {
				return nil, nil, derr
			}
			cli, cerr := docker.NewRemoteClient(func(_ context.Context, _, _ string) (net.Conn, error) {
				return cl.Dial("unix", "/var/run/docker.sock")
			})
			if cerr != nil {
				_ = cl.Close()
				return nil, nil, cerr
			}
			return cli, func() error { cli.Close(); return cl.Close() }, nil
		})
	}

	sched := backup.NewScheduler(backupStore, func(ctx context.Context, id int64) {
		// Bound scheduled runs like the manual path (backup_handlers.go) — an
		// unreachable S3/DB must not pin a run (and its pg_dump exec) forever.
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		if err := backupSvc.RunBackup(ctx, id, time.Now()); errors.Is(err, backup.ErrBackupRunning) {
			slog.Warn("scheduled backup skipped — previous run still in flight", "backup", id)
		} else if err != nil {
			slog.Error("scheduled backup failed", "backup", id, "err", err)
		}
	})
	if err := sched.Reload(); err != nil {
		slog.Warn("backup scheduler reload failed", "err", err)
	}
	defer sched.Stop()

	// Volume backups: separate service + a second in-process cron scheduler.
	volStore := volume.NewDBStore(q)
	volSvc := volume.New(engine, volStore, cfg.AllowPrivateEgress)
	volSvc.SetNotifier(notifySvc)
	volSched := backup.NewScheduler(volStore, func(ctx context.Context, id int64) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		if err := volSvc.RunVolumeBackup(ctx, id, time.Now()); errors.Is(err, volume.ErrVolumeBackupRunning) {
			slog.Warn("scheduled volume backup skipped — previous run still in flight", "volume_backup", id)
		} else if err != nil {
			slog.Error("scheduled volume backup failed", "volume_backup", id, "err", err)
		}
	})
	if err := volSched.Reload(); err != nil {
		slog.Warn("volume backup scheduler reload failed", "err", err)
	}
	defer volSched.Stop()

	// HTTP server.
	app := server.New(cfg, authSvc, orgSvc, q, dep, engine, hub, dbSvc)
	app.SetBackups(backupSvc, func() {
		if err := sched.Reload(); err != nil {
			slog.Error("backup scheduler reload failed", "err", err)
		}
	})
	app.SetVolumeBackups(volSvc, func() {
		if err := volSched.Reload(); err != nil {
			slog.Error("volume backup scheduler reload failed", "err", err)
		}
	})
	app.SetNotify(notifySvc)
	app.SetMetrics(metricsStore)
	app.SetSelfComponentFn(metricsSampler.SelfComponent)
	app.SetGatewayReconcile(startGatewayReconciler(ctx, engine, q, cfg.Network, acme))

	// Agent-facing API: REST (/api/v1) and MCP (/mcp) over the same twelve
	// operations, the same bearer tokens and the same tenancy checks in
	// internal/api. KRILL_AGENT_API_ENABLED=false leaves the authenticator unwired,
	// which unmounts both surfaces (RequireAPIToken 404s) — the whole agent
	// surface is off on an install that doesn't want it.
	if cfg.AgentAPIEnabled {
		app.SetAPI(api.NewAuthenticator(q, orgSvc), api.NewService(q, engine, dep))
		// An API token is a bearer credential: whoever reads one off the wire can
		// replay it until it is revoked. Over plain HTTP a single interception —
		// any hop between the agent and this process — is enough, so say so at
		// startup rather than serving tokens in clear text silently.
		if !strings.HasPrefix(cfg.BaseURL(), "https://") {
			slog.Warn("agent API enabled but the public URL is not https; bearer tokens will cross the network in clear text",
				"base_url", cfg.BaseURL())
		}
	} else {
		slog.Info("agent API disabled (KRILL_AGENT_API_ENABLED=false)")
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: app.Router(),
		// Slowloris guard on the request line/headers only; body/stream reads and
		// writes stay unbounded (long-lived WS: deploy logs, log viewer, terminal).
		// Do NOT set ReadTimeout/WriteTimeout — those would cut those streams.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("krill listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// An install that predates per-organization networks has every service —
	// every tenant's — on the single shared overlay, where any container can
	// resolve and reach any other by name. Nothing but a redeploy moves a Swarm
	// service between networks, so move them, once, in the background.
	//
	// In the background and AFTER the listener on purpose. The pass waits: on
	// each batch of databases to come back up, and on room in the shared deploy
	// queue. On a large install that is minutes, and blocking the boot on it
	// would leave the operator with no UI, no logs and no signal handler —
	// nothing to look at while it ran, and nothing but a kill to get out of it.
	// The pass is idempotent and already tolerates dying mid-way, so the worst a
	// shutdown here costs is redoing it next boot.
	//
	// It runs only when the gateway reconcile above actually succeeded:
	// Reconcile returns on the first NetworkEnsure error without deploying
	// anything, so migrating after a failed one would move services into
	// networks Traefik is not attached to — every exposed app unroutable until
	// some later, luckier start.
	if !gatewayReady {
		slog.Warn("skipping the organization network migration: the gateway is not attached to the organization networks")
	} else {
		go runOrgNetworkMigration(ctx, q, engine, dep, dbSvc)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-stop:
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// orgNetworks is the overlay network of every organization, in id order. The
// names are derived from the organization id rather than read from the stored
// network_name column: on an install that predates per-organization networks
// the column is still empty, and the gateway has to be attached to those
// networks before the startup migration moves any service into them.
func orgNetworks(ctx context.Context, q *db.Queries) ([]string, error) {
	orgs, err := q.ListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	nets := make([]string, 0, len(orgs))
	for _, o := range orgs {
		nets = append(nets, orgnet.Name(o.ID))
	}
	return nets, nil
}

// Timeouts for the one-shot startup migration onto per-organization networks.
const (
	orgNetMigrationTimeout    = 30 * time.Minute
	orgNetInstanceWaitTimeout = 3 * time.Minute
	orgNetInstanceWaitPoll    = 2 * time.Second
)

// runOrgNetworkMigration wires the migrator to the deployer, the DB-instance
// service and the engine, and runs one pass.
func runOrgNetworkMigration(ctx context.Context, q *db.Queries, engine docker.Engine, dep *deploy.Deployer, dbSvc *dbservice.Service) {
	// Bound the whole pass: EnqueueSystem waits for room in the shared deploy
	// queue, so without a deadline a jammed queue would hold startup forever.
	mctx, cancel := context.WithTimeout(ctx, orgNetMigrationTimeout)
	defer cancel()

	migrator := &orgnet.Migrator{
		Store: orgnet.NewDBStore(q),
		EnsureNetwork: func(c context.Context, name string) error {
			_, err := engine.NetworkEnsure(c, name)
			return err
		},
		// Leave stopped and never-deployed applications alone: an app the
		// operator scaled to zero must not come back up because the control
		// plane was upgraded.
		FilterApps: orgnet.RunningAppsFilter(engine),
		RedeployInstance: func(_ context.Context, id int64) error {
			dbSvc.DeployInstance(id) // asynchronous; WaitInstances below is what orders it
			return nil
		},
		WaitInstances: func(c context.Context, ids []int64) error {
			names := make([]string, 0, len(ids))
			for _, id := range ids {
				inst, err := q.GetDBInstance(c, id)
				if err != nil {
					return err
				}
				names = append(names, inst.AppName)
			}
			return orgnet.WaitServicesRunning(c, engine, names, orgNetInstanceWaitPoll, orgNetInstanceWaitTimeout)
		},
		RedeployApp: func(c context.Context, id int64) error {
			// EnqueueSystem, not Enqueue: the per-app and per-organization caps
			// would silently refuse most of a large organization's apps and leave
			// them stranded on the shared network.
			if dep.EnqueueSystem(c, id) == 0 {
				return errors.New("deploy could not be queued")
			}
			return nil
		},
	}
	if err := migrator.Run(mctx); err != nil {
		slog.Error("organization network migration failed (continuing)", "err", err)
	}
}

// gatewayReconcileDebounce collapses a burst of organization creations into one
// Traefik deploy; gatewayReconcileCooldown is the minimum time between two
// deploys. Together they bound the gateway to at most one task recreation per
// cooldown, however fast organizations are created — a debounce alone only
// rate-limits the abuse, because every new organization genuinely changes the
// network set and so cannot be absorbed by the spec fingerprint.
const (
	gatewayReconcileDebounce = 3 * time.Second
	gatewayReconcileCooldown = time.Minute
)

// startGatewayReconciler runs Traefik reconciliation in the background and
// returns the trigger the HTTP handlers call.
//
// Creating an organization needs no role — any authenticated user can POST
// /orgs — and reconciling recreates the Traefik task, rebinding :80/:443. On
// the request path that turns a loop of organization creations into a sustained
// outage for every tenant, so the work moves here, where a burst collapses into
// a single deploy after a quiet period.
func startGatewayReconciler(ctx context.Context, engine docker.Engine, q *db.Queries, baseNetwork string, acme traefik.AcmeConfig) func() {
	trigger := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(gatewayReconcileDebounce):
			}
			// Drop any triggers that arrived during the quiet period: this pass
			// reads the organization list fresh, so it already covers them.
			select {
			case <-trigger:
			default:
			}
			nets, err := orgNetworks(ctx, q)
			if err != nil {
				slog.Warn("gateway reconcile: could not list organization networks", "err", err)
				continue
			}
			if err := traefik.Reconcile(ctx, engine, baseNetwork, nets, acme); err != nil {
				slog.Error("gateway reconcile failed", "err", err)
			}
			// Hold the floor for the cooldown. Triggers that arrive meanwhile
			// stay buffered and are served by the next pass, which reads the
			// organization list fresh — so nothing is lost, it is only delayed.
			select {
			case <-ctx.Done():
				return
			case <-time.After(gatewayReconcileCooldown):
			}
		}
	}()
	return func() {
		select {
		case trigger <- struct{}{}:
		default: // one pending reconcile is enough
		}
	}
}
