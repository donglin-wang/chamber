package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	daemonconfig "github.com/donglin-wang/chamber/daemon/config"
	"github.com/donglin-wang/chamber/daemon/metadata/etcd"
	"github.com/donglin-wang/chamber/internal/bundle/umoci"
	"github.com/donglin-wang/chamber/internal/image/gocontainerregistry"
	"github.com/donglin-wang/chamber/internal/runtime/runc"
	"github.com/donglin-wang/chamber/internal/shared/localfs"
)

type startupOptions struct {
	configPath string
	override   daemonconfig.Override
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	options, err := parseArgs(args)
	if err != nil {
		return err
	}

	cfg, err := daemonconfig.LoadFile(options.configPath, options.override, os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	lifetime, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	directoryManager := localfs.NewDirectoryManager()
	store, err := etcd.Open(lifetime, cfg.Metadata, directoryManager)
	if err != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer store.Close()

	runtime := runc.New(cfg.Runtime, directoryManager)
	if _, err := runtime.Ensure(lifetime); err != nil {
		return fmt.Errorf("ensure runtime: %w", err)
	}

	mux := newServer()
	registerImageRoutes(mux, cfg, store, gocontainerregistry.New(directoryManager))
	registerContainerRoutes(
		mux,
		cfg,
		store,
		runtime,
		umoci.Provisioner{
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

	log.Printf("chamber daemon HTTP server listening on http://%s", cfg.HTTPAddr)
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
