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
	acme := traefik.AcmeConfig{Email: cfg.AcmeEmail, Staging: cfg.AcmeStaging}
	if acme.Email == "" {
		acme.Email = cfg.AdminEmail // fallback contact for Let's Encrypt
	}
	if err := traefik.Bootstrap(ctx, engine, cfg.Network, acme); err != nil {
		slog.Warn("traefik bootstrap failed (continuing)", "err", err)
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

	dep.Start(ctx)
	defer dep.Stop()

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
