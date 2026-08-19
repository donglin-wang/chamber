package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/donglin-wang/chamber/pkg/shared/logging"
)

const gracefulShutdownTimeout = 30 * time.Second

func main() {
	logging.SetLogger(logging.NewJSONLogger(os.Stderr, slog.LevelInfo))
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		logging.Error(context.Background(), "invalid webhook configuration", "error", err)
		os.Exit(1)
	}
	if !cfg.SkipPreflight {
		if err := runPreflight(context.Background(), cfg); err != nil {
			logging.Error(context.Background(), "webhook preflight failed", "error", err)
			os.Exit(1)
		}
	}
	server := newWebhookServer(cfg)
	if err := server.recoverIncompleteRuns(context.Background()); err != nil {
		logging.Error(context.Background(), "recover incomplete CI runs failed", "error", err)
		os.Exit(1)
	}
	logging.Info(context.Background(), "starting GitHub CI receiver", "addr", cfg.Addr, "root", cfg.Root, "repository", cfg.Repository)
	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: server.routes(),
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- httpServer.ListenAndServe()
	}()

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serverErr:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		logging.Error(context.Background(), "webhook receiver stopped", "error", err)
		os.Exit(1)
	case <-runCtx.Done():
		logging.Info(context.Background(), "stopping GitHub CI receiver")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancel()
	server.cancelActiveRuns()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logging.Error(context.Background(), "shutdown GitHub CI receiver failed", "error", err)
		os.Exit(1)
	}
	if err := server.waitForRuns(shutdownCtx); err != nil {
		logging.Error(context.Background(), "wait for active CI runs failed", "error", err)
		os.Exit(1)
	}
	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		logging.Error(context.Background(), "webhook receiver stopped", "error", err)
		os.Exit(1)
	}
}
