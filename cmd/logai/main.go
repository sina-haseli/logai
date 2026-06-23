package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/hibiken/asynq"

	"github.com/yourorg/logai/internal/anthropic"
	"github.com/yourorg/logai/internal/api"
	"github.com/yourorg/logai/internal/config"
	"github.com/yourorg/logai/internal/db"
	"github.com/yourorg/logai/internal/gitlab"
	"github.com/yourorg/logai/internal/ingestion"
	"github.com/yourorg/logai/internal/pipeline"
)

func main() {
	migrateOnly := flag.Bool("migrate-only", false, "apply the DB schema and exit")
	flag.Parse()

	if err := run(*migrateOnly); err != nil {
		// run already logged context; ensure a non-zero exit.
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(migrateOnly bool) error {
	cfg, err := config.Load()
	if err != nil {
		// Config errors happen before the logger is configured; print plainly.
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	// Root context cancelled on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Database ---
	database, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := database.Close(); cerr != nil {
			logger.Error("db close", "err", cerr)
		}
	}()

	if err := database.Migrate(ctx); err != nil {
		return err
	}
	logger.Info("database ready", "path", cfg.DBPath)

	if migrateOnly {
		logger.Info("migration complete; exiting (-migrate-only)")
		return nil
	}

	// --- Redis / Asynq connection ---
	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		logger.Error("invalid REDIS_URL", "err", err)
		os.Exit(1)
	}

	asynqClient := asynq.NewClient(redisOpt)
	defer asynqClient.Close()

	inspector := asynq.NewInspector(redisOpt)
	defer inspector.Close()

	// --- Reusable clients ---
	anthropicClient := anthropic.New(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL, logger)
	gitlabClient := gitlab.New(cfg.GitLabURL, cfg.GitLabToken, cfg.GitLabProjectID, logger)

	// --- Pipeline worker ---
	processor := pipeline.NewProcessor(database, anthropicClient, gitlabClient, cfg, logger)

	asynqServer := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: pipeline.WorkerConcurrency,
		Logger:      newAsynqLogger(logger),
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc(pipeline.TaskProcessError, processor.HandleProcessError)

	if err := asynqServer.Start(mux); err != nil {
		return err
	}
	logger.Info("asynq worker started", "concurrency", pipeline.WorkerConcurrency)

	// --- Ingestion ---
	ingestor := ingestion.NewIngestor(database, asynqClient, logger)

	poller, err := ingestion.NewOpenSearchPoller(cfg, ingestor, logger)
	if err != nil {
		asynqServer.Stop()
		asynqServer.Shutdown()
		return err
	}
	if err := poller.Start(ctx); err != nil {
		asynqServer.Stop()
		asynqServer.Shutdown()
		return err
	}

	// --- HTTP server ---
	apiHandlers := api.New(database, asynqClient, inspector, logger)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Post("/webhook/error", ingestor.WebhookHandler())
	apiHandlers.RegisterRoutes(r)

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// --- Wait for shutdown signal or fatal server error ---
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		logger.Error("http server error", "err", err)
	}

	// --- Graceful shutdown ---
	logger.Info("shutting down...")

	// 1. Stop accepting new ingestion work.
	poller.Stop()
	logger.Info("opensearch poller stopped")

	// 2. Stop the worker (finish in-flight tasks, stop pulling new ones).
	asynqServer.Stop()
	asynqServer.Shutdown()
	logger.Info("asynq worker stopped")

	// 3. Shut down the HTTP server with a 10s deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	} else {
		logger.Info("http server stopped")
	}

	// 4. DB + Redis clients close via deferred calls.
	logger.Info("shutdown complete")
	return nil
}

// newLogger builds a JSON slog logger at the configured level.
func newLogger(level string) *slog.Logger {
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
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// asynqSlogAdapter adapts slog to asynq.Logger.
type asynqSlogAdapter struct{ l *slog.Logger }

func newAsynqLogger(l *slog.Logger) asynq.Logger {
	return &asynqSlogAdapter{l: l.With("component", "asynq")}
}

func (a *asynqSlogAdapter) Debug(args ...any) { a.l.Debug("asynq", "msg", args) }
func (a *asynqSlogAdapter) Info(args ...any)  { a.l.Info("asynq", "msg", args) }
func (a *asynqSlogAdapter) Warn(args ...any)  { a.l.Warn("asynq", "msg", args) }
func (a *asynqSlogAdapter) Error(args ...any) { a.l.Error("asynq", "msg", args) }
func (a *asynqSlogAdapter) Fatal(args ...any) { a.l.Error("asynq-fatal", "msg", args) }
