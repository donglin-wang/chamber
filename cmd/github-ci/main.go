package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/donglin-wang/chamber/pkg/shared/logging"
)

func main() {
	logging.SetLogger(logging.NewJSONLogger(os.Stderr, slog.LevelInfo))
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
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
	logging.Info(context.Background(), "starting GitHub CI receiver", "addr", cfg.Addr, "root", cfg.Root, "repository", cfg.Repository)
	if err := http.ListenAndServe(cfg.Addr, server.routes()); err != nil {
		logging.Error(context.Background(), "webhook receiver stopped", "error", err)
		os.Exit(1)
	}
}
