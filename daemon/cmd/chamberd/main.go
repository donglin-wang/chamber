package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/donglin-wang/chamber/daemon"
	"github.com/donglin-wang/chamber/daemon/api"
	daemonconfig "github.com/donglin-wang/chamber/daemon/config"
	"github.com/donglin-wang/chamber/internal/bundle/umoci"
	"github.com/donglin-wang/chamber/internal/image/gocontainerregistry"
	"github.com/donglin-wang/chamber/internal/metadata/etcd"
	"github.com/donglin-wang/chamber/internal/runtime/runc"
	"github.com/donglin-wang/chamber/internal/shared/localfs"
)

const (
	gracefulShutdownTimeout = 5 * time.Second
	staleSocketDialTimeout  = 200 * time.Millisecond
)

type startupOptions struct {
	override daemonconfig.Override
	platform string
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "chamberd failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	options, err := parseArgs(args)
	if err != nil {
		return err
	}

	cfg, err := daemonconfig.Load(options.override, os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	directories := localfs.NewDirectoryManager()
	if err := prepareDaemonPaths(cfg.SocketPath, cfg.TmpRoot, directories); err != nil {
		return err
	}

	logger, err := newLogger(os.Stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	if err := runtimePreflight(runtime.GOOS, os.Geteuid()); err != nil {
		return err
	}
	if err := rootlessPreflight(os.ReadFile); err != nil {
		return err
	}

	lifetime, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	store, err := etcd.Open(lifetime, cfg.Metadata, localfs.NewDirectoryManager())
	if err != nil {
		return fmt.Errorf("open metadata store: %w", err)
	}
	defer store.Close()

	runtimeAdapter := runc.New(cfg.Runtime, localfs.NewDirectoryManager())
	binary, err := runtimeAdapter.Ensure(lifetime)
	if err != nil {
		return fmt.Errorf("ensure runtime: %w", err)
	}

	service := &daemon.Service{
		Store:   store,
		Puller:  gocontainerregistry.New(localfs.NewDirectoryManager()),
		Runtime: runtimeAdapter,
		Binary:  binary,
		Provisioner: umoci.Provisioner{
			ContainerRoot:    cfg.ContainerRoot,
			UID:              uint32(os.Geteuid()),
			GID:              uint32(os.Getegid()),
			DirectoryManager: localfs.NewDirectoryManager(),
		},
		IDs:           cryptoHexIDGenerator{},
		Lifetime:      lifetime,
		Logger:        logger,
		Directories:   directories,
		ImageRoot:     cfg.Image.Root,
		RuntimeRoot:   cfg.Runtime.RuntimeRoot,
		ContainerRoot: cfg.ContainerRoot,
		Platform:      options.platform,
	}

	if err := prepareUnixSocket(cfg.SocketPath, os.Geteuid()); err != nil {
		return err
	}
	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on unix socket: %w", err)
	}
	defer os.Remove(cfg.SocketPath)
	if err := os.Chmod(cfg.SocketPath, 0600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("secure unix socket: %w", err)
	}

	server := &http.Server{
		Handler: api.NewHandler(service),
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	logger.Info("chamberd started")

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve daemon API: %w", err)
	case <-lifetime.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown daemon API: %w", err)
	}
	err = <-serveErr
	if errors.Is(err, http.ErrServerClosed) {
		logger.Info("chamberd stopped")
		return nil
	}
	return err
}

func prepareDaemonPaths(socketPath string, tmpRoot string, directories localfs.DirectoryManager) error {
	if directories == nil {
		return fmt.Errorf("directory manager is required")
	}
	if err := directories.EnsurePrivateParent(socketPath); err != nil {
		return fmt.Errorf("prepare socket directory: %w", err)
	}
	if err := directories.EnsurePrivateDir(tmpRoot); err != nil {
		return fmt.Errorf("prepare tmp root: %w", err)
	}
	return nil
}

func parseArgs(args []string) (startupOptions, error) {
	var (
		options       startupOptions
		socketPath    string
		tmpRoot       string
		containerRoot string
		imageRoot     string
		runtimeRoot   string
		runtimeBinDir string
		runtimeName   string
		runtimeVer    string
		runtimeURL    string
		runtimeSHA256 string
		metadataRoot  string
		logLevel      string
		logFormat     string
	)

	fs := flag.NewFlagSet("chamberd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&socketPath, "socket-path", "", "Unix socket path")
	fs.StringVar(&tmpRoot, "tmp-root", "", "temporary root")
	fs.StringVar(&containerRoot, "container-root", "", "container root")
	fs.StringVar(&imageRoot, "image-root", "", "image root")
	fs.StringVar(&runtimeRoot, "runtime-root", "", "runtime state root")
	fs.StringVar(&runtimeBinDir, "runtime-bin-dir", "", "runtime binary directory")
	fs.StringVar(&runtimeName, "runtime-name", "", "runtime binary name")
	fs.StringVar(&runtimeVer, "runtime-version", "", "runtime version")
	fs.StringVar(&runtimeURL, "runtime-url", "", "runtime download URL")
	fs.StringVar(&runtimeSHA256, "runtime-sha256", "", "runtime binary SHA-256")
	fs.StringVar(&metadataRoot, "metadata-root", "", "metadata root")
	fs.StringVar(&logLevel, "log-level", "", "log level")
	fs.StringVar(&logFormat, "log-format", "", "log format")
	fs.StringVar(&options.platform, "platform", "", "image platform")

	if err := fs.Parse(args); err != nil {
		return startupOptions{}, err
	}
	if fs.NArg() != 0 {
		return startupOptions{}, fmt.Errorf("unexpected positional arguments")
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "socket-path":
			options.override.SocketPath = &socketPath
		case "tmp-root":
			options.override.TmpRoot = &tmpRoot
		case "container-root":
			options.override.ContainerRoot = &containerRoot
		case "image-root":
			options.override.Image.Root = &imageRoot
		case "runtime-root":
			options.override.Runtime.RuntimeRoot = &runtimeRoot
		case "runtime-bin-dir":
			options.override.Runtime.RuntimeBinDir = &runtimeBinDir
		case "runtime-name":
			options.override.Runtime.Name = &runtimeName
		case "runtime-version":
			options.override.Runtime.Version = &runtimeVer
		case "runtime-url":
			options.override.Runtime.URL = &runtimeURL
		case "runtime-sha256":
			options.override.Runtime.SHA256 = &runtimeSHA256
		case "metadata-root":
			options.override.Metadata.Root = &metadataRoot
		case "log-level":
			options.override.LogLevel = &logLevel
		case "log-format":
			options.override.LogFormat = &logFormat
		}
	})

	return options, nil
}

func newLogger(output io.Writer, level string, format string) (*slog.Logger, error) {
	slogLevel, err := parseLogLevel(level)
	if err != nil {
		return nil, err
	}
	options := &slog.HandlerOptions{
		Level: slogLevel,
	}

	switch strings.ToLower(format) {
	case "", "json":
		return slog.New(slog.NewJSONHandler(output, options)), nil
	case "text", "console":
		return slog.New(slog.NewTextHandler(output, options)), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q", format)
	}
}

func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", level)
	}
}

func runtimePreflight(goos string, euid int) error {
	if err := validateLinux(goos); err != nil {
		return err
	}
	if err := validateNonRoot(euid); err != nil {
		return err
	}
	return nil
}

type readFileFunc func(string) ([]byte, error)

func rootlessPreflight(readFile readFileFunc) error {
	if err := requireSysctlEnabled(readFile, "/proc/sys/kernel/unprivileged_userns_clone", "unprivileged user namespaces are disabled"); err != nil {
		return err
	}
	if err := requireSysctlPositive(readFile, "/proc/sys/user/max_user_namespaces", "user namespaces are unavailable"); err != nil {
		return err
	}
	if err := requireSysctlDisabled(readFile, "/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "unprivileged user namespaces are restricted by AppArmor"); err != nil {
		return err
	}
	return nil
}

func requireSysctlEnabled(readFile readFileFunc, path string, message string) error {
	value, ok, err := readOptionalInt(readFile, path)
	if err != nil || !ok {
		return err
	}
	if value == 0 {
		return errors.New(message)
	}
	return nil
}

func requireSysctlPositive(readFile readFileFunc, path string, message string) error {
	value, ok, err := readOptionalInt(readFile, path)
	if err != nil || !ok {
		return err
	}
	if value <= 0 {
		return errors.New(message)
	}
	return nil
}

func requireSysctlDisabled(readFile readFileFunc, path string, message string) error {
	value, ok, err := readOptionalInt(readFile, path)
	if err != nil || !ok {
		return err
	}
	if value != 0 {
		return errors.New(message)
	}
	return nil
}

func readOptionalInt(readFile readFileFunc, path string) (int, bool, error) {
	content, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read rootless prerequisite %s: %w", path, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		return 0, false, fmt.Errorf("parse rootless prerequisite %s: %w", path, err)
	}
	return value, true, nil
}

func validateLinux(goos string) error {
	if goos != "linux" {
		return fmt.Errorf("chamberd requires linux")
	}
	return nil
}

func validateNonRoot(euid int) error {
	if euid == 0 {
		return fmt.Errorf("chamberd must not run as root")
	}
	return nil
}

func prepareUnixSocket(path string, uid int) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect unix socket: %w", err)
	}

	conn, dialErr := net.DialTimeout("unix", path, staleSocketDialTimeout)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("unix socket already has a listening server")
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket path exists and is not a unix socket")
	}
	ownerUID, ok := fileOwnerUID(info)
	if !ok {
		return fmt.Errorf("cannot determine unix socket owner")
	}
	if ownerUID != uid {
		return fmt.Errorf("stale unix socket is owned by another user")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale unix socket: %w", err)
	}
	return nil
}

func fileOwnerUID(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

type cryptoHexIDGenerator struct{}

func (cryptoHexIDGenerator) New() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(fmt.Sprintf("generate random id: %v", err))
	}
	return "c" + hex.EncodeToString(random[:])
}
