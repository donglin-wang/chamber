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
	"path/filepath"
	"syscall"
	"time"

	chamberDaemonConfig "github.com/donglin-wang/chamber/daemon/config"
	chamberEtcdMetadataStore "github.com/donglin-wang/chamber/daemon/metadata/etcd"
	chamberBundleFactory "github.com/donglin-wang/chamber/pkg/bundle/factory"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type startupOptions struct {
	configPath string
	input      chamberDaemonConfig.Input
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

	cfg, err := chamberDaemonConfig.LoadFile(options.configPath, options.input, os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	configureLogging(cfg.Logging)

	lifetime, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	imageConfig := cfg.Image
	imageConfig.TmpRoot = filepath.Join(cfg.TmpRoot, "images")
	bundleConfig := cfg.Bundle
	bundleConfig.TmpRoot = filepath.Join(cfg.TmpRoot, "bundles")
	runtimeConfig := cfg.Runtime
	runtimeConfig.RuntimeTmpRoot = filepath.Join(cfg.TmpRoot, "runtime")
	runtimeConfig.RuntimeBinTmpRoot = filepath.Join(cfg.TmpRoot, "runtime-bin")

	imageWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    imageConfig.Root,
		TmpRoot: imageConfig.TmpRoot,
		Capabilities: hostfs.Capabilities{
			PrivateDirs:           true,
			FileFsync:             true,
			AtomicFileRename:      true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		return fmt.Errorf("create image workspace: %w", err)
	}
	bundleWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    bundleConfig.Root,
		TmpRoot: bundleConfig.TmpRoot,
		Capabilities: hostfs.Capabilities{
			PrivateDirs:           true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		return fmt.Errorf("create bundle workspace: %w", err)
	}
	runtimeWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    runtimeConfig.RuntimeRoot,
		TmpRoot: runtimeConfig.RuntimeTmpRoot,
		Capabilities: hostfs.Capabilities{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		return fmt.Errorf("create runtime workspace: %w", err)
	}
	var runtimeBinaryWorkspace *hostfs.Workspace
	if runtimeConfig.RuntimePath == "" {
		runtimeBinaryWorkspace, err = hostfs.NewWorkspace(hostfs.Config{
			Root:    runtimeConfig.RuntimeBinDir,
			TmpRoot: runtimeConfig.RuntimeBinTmpRoot,
			Capabilities: hostfs.Capabilities{
				PrivateDirs:      true,
				FileFsync:        true,
				AtomicFileRename: true,
			},
		})
		if err != nil {
			return fmt.Errorf("create runtime binary workspace: %w", err)
		}
	}
	metadataWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    cfg.Metadata.Root,
		TmpRoot: filepath.Join(cfg.TmpRoot, "metadata"),
		Capabilities: hostfs.Capabilities{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
			DirectoryFsync:   true,
		},
	})
	if err != nil {
		return fmt.Errorf("create metadata workspace: %w", err)
	}

	store, err := chamberEtcdMetadataStore.Open(lifetime, cfg.Metadata, metadataWorkspace)
	if err != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer store.Close()

	runtime, err := chamberRuntimeFactory.NewRuntime(lifetime, runtimeConfig, runtimeWorkspace, runtimeBinaryWorkspace)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}

	mux := newServer()
	imageStore, err := chamberImageFactory.NewStore(imageConfig, imageWorkspace)
	if err != nil {
		return fmt.Errorf("create image store: %w", err)
	}
	registerImageRoutes(mux, cfg, store, imageStore)
	provisioner, err := chamberBundleFactory.NewProvisioner(
		bundleConfig,
		bundleWorkspace,
	)
	if err != nil {
		return fmt.Errorf("create bundle provisioner: %w", err)
	}
	registerContainerRoutes(
		mux,
		store,
		imageStore,
		runtime,
		provisioner,
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
			options.input.HTTPAddr = &httpAddr
		}
	})

	return options, nil
}
