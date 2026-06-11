package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/builder"
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
	b := builder.New(cfg.DockerHost)
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

	dep := deploy.New(engine, b, store, hub, cfg.Network)
	dep.SetNotifier(notifySvc)
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

	// Backups: service + in-process cron scheduler.
	backupStore := backup.NewDBStore(q)
	backupSvc := backup.New(engine, backupStore)
	backupSvc.SetNotifier(notifySvc)

	// Health watcher: polls service state for app down/recovered alerts.
	watcher := notify.NewWatcher(engine, notifyStore, notifySvc, cfg.HealthPollInterval)
	go watcher.Run(ctx)

	// Monitoring: sample container stats into Postgres for the Monitoring page.
	metricsStore := metrics.NewDBStore(q)
	go metrics.NewSampler(engine, metricsStore, cfg.MetricsInterval, cfg.MetricsRetention).Run(ctx)

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

	// HTTP server.
	app := server.New(cfg, authSvc, orgSvc, q, dep, engine, hub, dbSvc)
	app.SetBackups(backupSvc, func() {
		if err := sched.Reload(); err != nil {
			slog.Error("backup scheduler reload failed", "err", err)
		}
	})
	app.SetNotify(notifySvc)
	app.SetMetrics(metricsStore)
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: app.Router(),
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
