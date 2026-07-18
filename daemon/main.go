package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	chamberDaemonConfig "github.com/donglin-wang/chamber/daemon/config"
	chamberEtcdMetadataStore "github.com/donglin-wang/chamber/daemon/metadata/etcd"
	chamberRootlessProvisioner "github.com/donglin-wang/chamber/pkg/bundle/rootless"
	chamberImagePuller "github.com/donglin-wang/chamber/pkg/image/puller"
	chamberRuncRuntime "github.com/donglin-wang/chamber/pkg/runtime/runc"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type startupOptions struct {
	configPath string
	override   chamberDaemonConfig.Override
}

func main() {
	configureLogging(chamberLogging.DefaultConfig())
	if err := run(context.Background(), os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		slog.Default().Error("chamber daemon failed", "error", err)
		os.Exit(1)
	}
}

func configureLogging(config chamberLogging.Config) {
	logger, err := chamberLogging.NewLogger(os.Stderr, config)
	if err != nil {
		logger = chamberLogging.NewJSONLogger(os.Stderr, slog.LevelInfo)
	}
	chamberLogging.SetLogger(logger)
	slog.SetDefault(logger)
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "storage" {
		return runStorage(args[1:], os.Getenv, os.Stdout)
	}

	options, err := parseArgs(args)
	if err != nil {
		return err
	}

	cfg, err := chamberDaemonConfig.LoadFile(options.configPath, options.override, os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	configureLogging(cfg.Logging)

	lifetime, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	directoryManager := localfs.NewDirectoryManager()
	store, err := chamberEtcdMetadataStore.Open(lifetime, cfg.Metadata, directoryManager)
	if err != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer store.Close()

	runtime := chamberRuncRuntime.New(cfg.Runtime, directoryManager)
	if _, err := runtime.Ensure(lifetime); err != nil {
		return fmt.Errorf("ensure runtime: %w", err)
	}

	mux := newServer()
	registerImageRoutes(mux, cfg, store, chamberImagePuller.New(cfg.Image, directoryManager))
	registerContainerRoutes(
		mux,
		cfg,
		store,
		runtime,
		chamberRootlessProvisioner.Provisioner{
			Config:           cfg.Bundle,
			UID:              uint32(os.Geteuid()),
			GID:              uint32(os.Getegid()),
			DirectoryManager: directoryManager,
		},
		lifetime,
	)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Default().Info("chamber daemon HTTP server listening", "http_addr", cfg.HTTPAddr)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("serve HTTP: %w", err)
		}
	case <-lifetime.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		if err := <-serveErr; err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("serve HTTP: %w", err)
		}
	}
	return nil
}

func parseArgs(args []string) (startupOptions, error) {
	var (
		options  startupOptions
		httpAddr string
	)

	fs := flag.NewFlagSet("chamberd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&options.configPath, "config", "", "JSON config file path")
	fs.StringVar(&httpAddr, "http-addr", "", "HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return startupOptions{}, err
	}
	if fs.NArg() != 0 {
		return startupOptions{}, fmt.Errorf("unexpected positional arguments")
	}

	fs.Visit(func(f *flag.Flag) {
		if f.Name == "http-addr" {
			options.override.HTTPAddr = &httpAddr
		}
	})

	return options, nil
}
