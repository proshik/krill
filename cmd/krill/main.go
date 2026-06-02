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
	"github.com/proshik/krill/internal/builder"
	"github.com/proshik/krill/internal/config"
	"github.com/proshik/krill/internal/database"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
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
	if err := traefik.Bootstrap(ctx, engine, cfg.Network); err != nil {
		slog.Warn("traefik bootstrap failed (continuing)", "err", err)
	}

	store := deploy.NewDBStore(q)
	dep := deploy.New(engine, b, store, hub, cfg.Network)
	dep.Start(ctx)
	defer dep.Stop()

	stopCleanup := deploy.StartLogCleanup(ctx, store, 10*time.Minute)
	defer stopCleanup()

	dbStore := dbservice.NewDBStore(q)
	dbSvc := dbservice.New(engine, dbStore, hub, cfg.Network)

	// HTTP server.
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server.New(cfg, authSvc, orgSvc, q, dep, engine, hub, dbSvc).Router(),
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
