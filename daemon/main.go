package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	chamberDaemonConfig "github.com/donglin-wang/chamber/daemon/config"
	chamberMetadata "github.com/donglin-wang/chamber/daemon/metadata"
	chamberEtcdMetadataStore "github.com/donglin-wang/chamber/daemon/metadata/etcd"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberBundleFactory "github.com/donglin-wang/chamber/pkg/bundle/factory"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
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
	if len(args) > 0 && args[0] == "runtime-supervisor" {
		return runRuntimeSupervisor(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == daemonActiveProbeCommand {
		if len(args) != 1 {
			return fmt.Errorf("%s does not accept arguments", daemonActiveProbeCommand)
		}
		if _, err := fmt.Fprintln(os.Stdout, daemonActiveProbeMarker); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		return nil
	}
	if len(args) > 0 && args[0] == "storage" {
		return runStorage(args[1:], os.Getenv, os.Stdout)
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
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
	startupProbe := newDaemonStartupProbe()

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
		Requirements: hostfs.FeatureSet{
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
		Requirements: hostfs.FeatureSet{
			PrivateDirs:           true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		return fmt.Errorf("create bundle workspace: %w", err)
	}
	metadataWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    cfg.Metadata.Root,
		TmpRoot: filepath.Join(cfg.TmpRoot, "metadata"),
		Requirements: hostfs.FeatureSet{
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
	if probeErr := startupProbe.record(daemonStartupScope{
		Name: "metadata", Implementation: "etcd", Path: cfg.Metadata.Root,
	}, err); probeErr != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer store.Close()

	mux := newServer()
	imageStore, err := chamberImageFactory.NewStoreWithWorkspace(imageConfig, imageWorkspace)
	if probeErr := startupProbe.record(daemonStartupScope{
		Name: "image", Implementation: "oci-layout", Path: imageConfig.Root,
	}, err); probeErr != nil {
		return fmt.Errorf("create image store: %w", err)
	}
	registerImageRoutes(mux, store, imageStore)
	provisioner, err := chamberBundleFactory.NewProvisionerWithWorkspace(
		bundleConfig,
		bundleWorkspace,
	)
	if err != nil {
		_ = startupProbe.record(daemonStartupScope{
			Name: "bundle", Implementation: bundleConfig.Name, Path: bundleConfig.Root,
		}, err)
		return fmt.Errorf("create bundle provisioner: %w", err)
	}
	rt, err := chamberRuntimeFactory.NewRuntime(lifetime, runtimeConfig)
	if err != nil {
		_ = startupProbe.record(daemonStartupScope{
			Name: "runtime", Implementation: runtimeConfig.Name, Path: runtimeConfig.RuntimeRoot,
		}, err)
		return fmt.Errorf("create runtime: %w", err)
	}
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve daemon executable for active startup probe: %w", err)
	}
	activeProbe := runDaemonActiveProbes(lifetime, cfg.TmpRoot, executablePath, provisioner, rt)
	if activeProbe.RecoveryErr != nil {
		probeErr := startupProbe.record(daemonStartupScope{
			Name: "cleanup", Implementation: "leased", Path: filepath.Join(filepath.Dir(cfg.Metadata.Root), "supervisors"),
		}, activeProbe.RecoveryErr)
		return fmt.Errorf("reclaim abandoned active startup probe: %w", probeErr)
	}
	if probeErr := startupProbe.record(daemonStartupScope{
		Name: "bundle", Implementation: bundleConfig.Name, Path: bundleConfig.Root,
	}, activeProbe.ProvisionErr); probeErr != nil {
		return errors.Join(probeErr, activeProbe.CleanupErr)
	}
	if probeErr := startupProbe.record(daemonStartupScope{
		Name: "runtime", Implementation: runtimeConfig.Name, Path: runtimeConfig.RuntimeRoot,
	}, activeProbe.RunErr); probeErr != nil {
		return errors.Join(probeErr, activeProbe.CleanupErr)
	}
	reconcileErr := reconcileDaemonState(lifetime, store, runtimeConfig, provisioner)
	if probeErr := startupProbe.record(daemonStartupScope{
		Name: "cleanup", Implementation: "leased", Path: filepath.Join(filepath.Dir(cfg.Metadata.Root), "supervisors"),
	}, errors.Join(activeProbe.CleanupErr, reconcileErr)); probeErr != nil {
		return probeErr
	}
	go watchSupervisorContainers(lifetime, store, startSupervisorProcess, openRuntimeContainer, runtimeConfig)
	go watchContainerCleanups(lifetime, store, runtimeConfig, provisioner, openRuntimeContainer, terminateSupervisorProcessGroup)
	registerOperationRoutes(mux, store)
	registerContainerRoutes(
		mux,
		store,
		imageStore,
		runtimeConfig,
		provisioner,
		filepath.Join(filepath.Dir(cfg.Metadata.Root), "supervisors"),
		startSupervisorProcess,
		openRuntimeContainer,
		terminateSupervisorProcessGroup,
	)

	listener, err := daemonListener(cfg)
	transportPath := cfg.SocketPath
	transportName := "unix"
	if cfg.HTTPAddr != "" {
		transportPath = cfg.HTTPAddr
		transportName = "tcp-development"
	}
	if probeErr := startupProbe.record(daemonStartupScope{
		Name: "socket", Implementation: transportName, Path: transportPath,
	}, err); probeErr != nil {
		return err
	}
	defer listener.Close()
	if cfg.HTTPAddr == "" {
		defer os.Remove(cfg.SocketPath)
	}
	registerSystemRoutes(mux, startupProbe.result())

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Default().Info("chamber daemon HTTP server listening", "addr", listener.Addr().String(), "network", listener.Addr().Network())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
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

func reconcileDaemonState(
	ctx context.Context,
	store chamberMetadata.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
) error {
	if err := reconcileSupervisorContainers(ctx, store, startSupervisorProcess, openRuntimeContainer, runtimeConfig, true); err != nil {
		return fmt.Errorf("reconcile supervisor containers: %w", err)
	}
	if err := reconcileContainerCleanups(ctx, store, runtimeConfig, provisioner, openRuntimeContainer, terminateSupervisorProcessGroup, true); err != nil {
		return fmt.Errorf("reconcile container cleanups: %w", err)
	}
	if err := reconcileRunningOperations(ctx, store); err != nil {
		return fmt.Errorf("reconcile running operations: %w", err)
	}
	return nil
}

func daemonListener(cfg chamberDaemonConfig.Config) (net.Listener, error) {
	if cfg.HTTPAddr != "" {
		listener, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return nil, fmt.Errorf("listen HTTP TCP: %w", err)
		}
		return listener, nil
	}
	if cfg.SocketPath == "" {
		return nil, fmt.Errorf("daemon socket path is required when http addr is empty")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o700); err != nil {
		return nil, fmt.Errorf("create daemon socket directory: %w", err)
	}
	if err := os.Remove(cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale daemon socket: %w", err)
	}
	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen HTTP Unix socket: %w", err)
	}
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("set daemon socket mode: %w", err)
	}
	return listener, nil
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
