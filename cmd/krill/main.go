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
	"github.com/proshik/krill/internal/buildinfo"
	"github.com/proshik/krill/internal/cluster"
	"github.com/proshik/krill/internal/config"
	"github.com/proshik/krill/internal/database"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/orgnet"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/selfupdate"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/traefik"
	"github.com/proshik/krill/internal/volume"
)

func main() {
	// Handled before run()/config.Load() so it works with no environment at
	// all — the self-updater uses this as a smoke test right after replacing
	// the binary, before it knows the new binary's configuration is sane.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "version") {
		fmt.Println("krill", buildinfo.String())
		return
	}
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
	slog.Info("starting krill", "version", buildinfo.String(), "listen", cfg.ListenAddr, "base_domain", cfg.BaseDomain, "network", cfg.Network)

	// Encryption-at-rest for stored credentials (opt-in via KRILL_SECRET_KEY).
	secret.Init(cfg.SecretKey)
	if !secret.Enabled() {
		slog.Warn("KRILL_SECRET_KEY not set — stored secrets (DB passwords, registry/destination credentials) are NOT encrypted at rest")
	}

	// HTTP listener. The port opens before migrations and the gateway reconcile
	// (which can take minutes), and until the router is ready the gate answers
	// every request with a 503 "Krill is starting" page instead of leaving the
	// browser with "connection refused".
	gate := server.NewStartupGate()
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: gate,
		// Slowloris guard on the request line/headers only; body/stream reads and
		// writes stay unbounded (long-lived WS: deploy logs, log viewer, terminal).
		// Do NOT set ReadTimeout/WriteTimeout — those would cut those streams.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Listen synchronously: a busy port must still fail the start right away,
	// not minutes later.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("krill listening", "addr", cfg.ListenAddr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Migrations on startup, under their own signal handler: the one below is
	// registered only once startup is done. Without it a SIGTERM during a slow
	// migration — the revert timer's restart, an operator's `systemctl stop` —
	// kills the process mid-migration and leaves the schema dirty, and no
	// binary, new or reverted, starts against a dirty schema. With it the
	// running migration finishes and startup ends with an error instead.
	gate.SetPhase(server.PhaseMigrating)
	migCtx, stopMig := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = database.RunMigrationsContext(migCtx, cfg.DatabaseURL)
	stopMig()
	if err != nil {
		return err
	}
	gate.SetPhase(server.PhaseStarting)

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

	// The panel domain: the gateway polls Krill for the UI's own routes, so it
	// needs an address it can reach Krill at. Without one the provider is left
	// out of the gateway spec and the Panel domain page explains why. The
	// provider's arguments come from configuration and a stored secret only, so
	// they are the same on every start and never make the gateway redeploy.
	gatewaySecret, err := panel.EnsureSecret(ctx, q)
	if err != nil {
		return err
	}
	gatewayTokens := panel.DeriveTokens(gatewaySecret)
	panelUpstream, panelUpstreamErr := panel.Upstream(cfg.AdvertiseAddr, cfg.ListenAddr)
	var panelProvider traefik.PanelProvider
	if panelUpstreamErr != nil {
		slog.Info("panel domain unavailable: the gateway cannot reach Krill", "reason", panelUpstreamErr)
	} else {
		panelProvider = traefik.PanelProvider{
			Endpoint: panelUpstream + panel.ProviderPath,
			Header:   panel.ProviderHeader,
			Token:    gatewayTokens.Provider,
		}
	}
	// The gateway has to sit in every organization's network, not just the base
	// one, or it cannot route to services that live behind the per-organization
	// isolation. Names come from orgnet.Name, not from the stored network_name:
	// on an install that has not been migrated yet the column is still empty,
	// and Traefik has to be in those networks BEFORE the migrator moves any
	// service into them.
	// A failed listing must not reconcile: an empty list is indistinguishable
	// from "this install has no organizations", and applying it would detach the
	// running gateway from every organization network over a transient DB error.
	// When the spec changed, Reconcile waits for the new Traefik task to run
	// (seconds normally, up to a few minutes if the image has to be pulled). The
	// HTTP listener is already up by then, serving the starting page.
	gate.SetPhase(server.PhaseGateway)
	gatewayReady := false
	if orgNets, nerr := orgNetworks(ctx, q); nerr != nil {
		slog.Warn("could not list organization networks; leaving the gateway as it is", "err", nerr)
	} else if rerr := traefik.Reconcile(ctx, engine, cfg.Network, orgNets, acme, panelProvider); rerr != nil {
		slog.Warn("traefik reconcile failed (continuing)", "err", rerr)
	} else {
		gatewayReady = true
	}
	gate.SetPhase(server.PhaseStarting)

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
	app.SetControlPlaneFirewall(firewall.LocalRunner{Timeout: 20 * time.Second})
	app.SetGatewayReconcile(startGatewayReconciler(ctx, engine, q, cfg.Network, acme, panelProvider))
	app.SetPanelGateway(gatewayTokens, panelUpstream, panelUpstreamErr)

	// Self-update: discover newer releases and install one from Settings → Updates.
	updateChecker := selfupdate.NewChecker(cfg.UpdateRepo, cfg.UpdateCheckInterval, cfg.AllowPrivateEgress)
	updater := selfupdate.NewUpdater(firewall.LocalRunner{Timeout: 60 * time.Second}, updateChecker,
		cfg.UpdateRepo, cfg.AllowPrivateEgress, updateBusy{
			runningDeploys:     q.CountRunningDeployments,
			migratingInstances: q.CountMigratingDBInstances,
			backups:            backupSvc,
			volumes:            volSvc,
		}.reason)
	app.SetSelfUpdate(updateChecker, updater)

	// Observability: Krill's own Alloy agent on every node (Settings →
	// Observability). The real engine implements the Swarm config/secret
	// capability; anything else leaves the page unwired.
	var obsRec *observability.Reconciler
	if obsEngine, ok := engine.(observability.Engine); ok {
		// obsNodes resolves the krill_node mapping fresh on every reconcile
		// pass. Reading both lists is required, not best-effort: a failure
		// here fails the whole reconcile pass (see NewReconciler) rather than
		// silently redeploying the agent without the mapping.
		obsNodes := func(c context.Context) ([]observability.NodeName, error) {
			swarmNodes, nerr := engine.Nodes(c)
			if nerr != nil {
				return nil, nerr
			}
			rows, rerr := q.ListNodeLabels(c)
			if rerr != nil {
				return nil, rerr
			}
			return obsNodeNames(swarmNodes, rows), nil
		}
		obsRec = observability.NewReconciler(obsEngine, func(c context.Context) (observability.Settings, error) {
			return observability.LoadForReconcile(c, q)
		}, obsNodes, cfg.Network)
		instance, _ := os.Hostname()
		checker := observability.Checker{AllowPrivate: cfg.AllowPrivateEgress, Instance: instance}
		app.SetObservability(obsRec, checker.Check)
	} else {
		slog.Warn("observability unavailable: the docker engine cannot manage swarm configs and secrets")
	}

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

	// A firewall change is confirmed by a request that the new ruleset had to
	// admit; an idle keep-alive connection from before it would not prove that.
	app.SetIdleConnCloser(func() {
		srv.SetKeepAlivesEnabled(false) // closes idle connections
		srv.SetKeepAlivesEnabled(true)
	})

	// Hand the listener to the real router — only through the gate: srv.Serve is
	// already running, and assigning srv.Handler now would race with it.
	gate.Swap(app.Router())
	slog.Info("krill serving")

	// A binary started by a self-update confirms itself once it serves the UI,
	// which disarms the dead-man timer that would otherwise put the previous
	// binary back. In the background: it runs a few systemctl calls and may wait
	// for a request that is still validating, and nothing after this point
	// (the network migration, the signal handler) should wait on it.
	go updater.ConfirmStartup(ctx)
	go updateChecker.Run(ctx)
	if cfg.UpdateCheckInterval <= 0 {
		slog.Info("background update check disabled (KRILL_UPDATE_CHECK_INTERVAL <= 0)")
	}

	// Converge the agent to the stored settings once per start: deploy it if
	// it is on, remove it (and its leftover configs and secrets) if it is off.
	// In the background — pulling the image can take minutes.
	if obsRec != nil {
		go obsRec.Run(ctx)
		obsRec.Trigger()
	}

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
	// It runs only when the gateway reconcile above actually succeeded, which
	// means Traefik's new task is running on the new network set — not merely
	// that Swarm accepted the spec. Migrating after a failed one would move
	// services into networks Traefik is not attached to — every exposed app
	// unroutable until some later, luckier start.
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

// obsNodeNames maps live Swarm nodes to the names Krill's observability agent
// labels them with — the exact same node_labels the Nodes page, the
// topology view and the app page already show (see (*server.Server).nodeLabelMap),
// not cluster_nodes.name (cluster_nodes holds only workers, so a manager
// would never get a name that way) and not a hardcoded name for the manager
// (that would silently rename krill_node on every existing install the
// moment it upgrades, before the operator ever names anything). A node with
// no label gets an empty Name: RenderNodeConfig leaves it out of the
// krill_node mapping (its krill_node keeps showing the raw Swarm hostname,
// exactly like an install that has never labelled any node — nothing changes
// silently), but Reconcile still needs it in the list, Expected flag and
// all, to know how many nodes the agent is still expected to reach on this
// pass (see Coverage) — a label is a display choice, not a reason to stop
// counting a node.
func obsNodeNames(swarmNodes []docker.SwarmNode, labels []db.NodeLabel) []observability.NodeName {
	labelByID := make(map[string]string, len(labels))
	for _, l := range labels {
		labelByID[l.SwarmNodeID] = l.Label
	}
	names := make([]observability.NodeName, 0, len(swarmNodes))
	for _, n := range swarmNodes {
		names = append(names, observability.NodeName{
			Hostname: n.Hostname,
			Name:     labelByID[n.ID],
			// A down or paused node still counts: Swarm leaves its last
			// container running there, so it's exactly the case Coverage
			// exists to name. Only a drained node has had its task actively
			// removed — see NodeName.Expected.
			Expected: n.Availability != "drain",
		})
	}
	return names
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
	orgNetDeployPoll          = 2 * time.Second
)

// runOrgNetworkMigration wires the migrator to the deployer, the DB-instance
// service and the engine, and runs one pass.
func runOrgNetworkMigration(ctx context.Context, q *db.Queries, engine docker.Engine, dep *deploy.Deployer, dbSvc *dbservice.Service) {
	// Bound the whole pass: EnqueueSystem waits for room in the shared deploy
	// queue and the migrator waits for every app deployment it submitted to
	// finish, so without a deadline a jammed queue or a stuck build would hold
	// the pass forever. Running out of time leaves the organization in progress
	// unmarked, and the next start retries it.
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
		// The same for database instances, except that a stopped one is moved
		// without being started rather than skipped: Start only scales the
		// existing service, so one left behind would come back up on the shared
		// network.
		PlanInstances: orgnet.RunningInstancesFilter(engine, func(c context.Context, id int64) (orgnet.InstanceRow, error) {
			inst, err := q.GetDBInstance(c, id)
			if err != nil {
				return orgnet.InstanceRow{}, err
			}
			return orgnet.InstanceRow{AppName: inst.AppName, Status: inst.Status}, nil
		}),
		ParkInstance: dbSvc.ParkInstance,
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
		RedeployApp: func(c context.Context, id int64) (int64, error) {
			// EnqueueSystem, not Enqueue: the per-app and per-organization caps
			// would silently refuse most of a large organization's apps and leave
			// them stranded on the shared network.
			depID := dep.EnqueueSystem(c, id)
			if depID == 0 {
				return 0, errors.New("deploy could not be queued")
			}
			return depID, nil
		},
		DeployPoll: orgNetDeployPoll,
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
func startGatewayReconciler(ctx context.Context, engine docker.Engine, q *db.Queries, baseNetwork string, acme traefik.AcmeConfig, panelProvider traefik.PanelProvider) func() {
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
			if err := traefik.Reconcile(ctx, engine, baseNetwork, nets, acme, panelProvider); err != nil {
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
