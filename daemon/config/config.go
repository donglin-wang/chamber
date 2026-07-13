package config

import (
	"fmt"
	"path/filepath"
	"time"

	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/donglin-wang/chamber/internal/metadata"
	chruntime "github.com/donglin-wang/chamber/internal/runtime"
	"github.com/donglin-wang/chamber/internal/shared/localfs"
)

type Config struct {
	// Storage
	SocketPath    string
	TmpRoot       string
	ContainerRoot string

	// Images
	Image chimage.Config

	// OCI Runtime
	Runtime chruntime.Config

	// Metadata
	Metadata metadata.Config

	// OpenTelemetry
	OpenTelemetryEndpoint              string
	OpenTelemetryInsecure              bool
	OpenTelemetryTraceSampleRatio      float64
	OpenTelemetryMetricsExportInterval time.Duration

	// Logging
	LogLevel  string
	LogFormat string
}

type Override struct {
	SocketPath    *string
	TmpRoot       *string
	ContainerRoot *string

	Image    chimage.Override
	Runtime  chruntime.Override
	Metadata metadata.Override

	OpenTelemetryEndpoint              *string
	OpenTelemetryInsecure              *bool
	OpenTelemetryTraceSampleRatio      *float64
	OpenTelemetryMetricsExportInterval *time.Duration

	LogLevel  *string
	LogFormat *string
}

const (
	defaultOpenTelemetryTraceSampleRatio      = 1.0
	defaultOpenTelemetryMetricsExportInterval = 10 * time.Second
	defaultLogLevel                           = "info"
	defaultLogFormat                          = "json"
)

func getRootPath(getenv func(string) string) string {
	rootPath := ""
	xdg := getenv("XDG_DATA_HOME")
	home := getenv("HOME")

	if xdg != "" {
		rootPath = filepath.Join(xdg, "chamber")
	} else if home != "" {
		rootPath = filepath.Join(home, ".local", "share", "chamber")
	} else {
		panic("cannot derive root path: neither $XDG_DATA_HOME nor $HOME are set")
	}

	absPath, err := filepath.Abs(rootPath)
	if err != nil {
		panic(fmt.Sprintf("cannot convert root path %q to absolute path: %v", rootPath, err))
	}

	return absPath
}

func Load(override Override, getenv func(string) string) (Config, error) {
	rootPath := getRootPath(getenv)

	defaultConfig := Config{
		SocketPath:    filepath.Join(rootPath, "run", "chamber.sock"),
		TmpRoot:       filepath.Join(rootPath, "run", "tmp"),
		ContainerRoot: filepath.Join(rootPath, "containers"),

		Image:    chimage.DefaultConfig(rootPath),
		Runtime:  chruntime.DefaultConfig(rootPath),
		Metadata: metadata.DefaultConfig(rootPath),

		OpenTelemetryTraceSampleRatio:      defaultOpenTelemetryTraceSampleRatio,
		OpenTelemetryMetricsExportInterval: defaultOpenTelemetryMetricsExportInterval,
		LogLevel:                           defaultLogLevel,
		LogFormat:                          defaultLogFormat,
	}

	return Resolve(defaultConfig, override)
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.SocketPath != nil {
		defaultConfig.SocketPath = *override.SocketPath
	}
	if override.TmpRoot != nil {
		defaultConfig.TmpRoot = *override.TmpRoot
	}
	if override.ContainerRoot != nil {
		defaultConfig.ContainerRoot = *override.ContainerRoot
	}
	if override.OpenTelemetryEndpoint != nil {
		defaultConfig.OpenTelemetryEndpoint = *override.OpenTelemetryEndpoint
	}
	if override.OpenTelemetryInsecure != nil {
		defaultConfig.OpenTelemetryInsecure = *override.OpenTelemetryInsecure
	}
	if override.OpenTelemetryTraceSampleRatio != nil {
		defaultConfig.OpenTelemetryTraceSampleRatio = *override.OpenTelemetryTraceSampleRatio
	}
	if override.OpenTelemetryMetricsExportInterval != nil {
		defaultConfig.OpenTelemetryMetricsExportInterval = *override.OpenTelemetryMetricsExportInterval
	}
	if override.LogLevel != nil {
		defaultConfig.LogLevel = *override.LogLevel
	}
	if override.LogFormat != nil {
		defaultConfig.LogFormat = *override.LogFormat
	}

	var err error
	defaultConfig.Image, err = chimage.Resolve(defaultConfig.Image, override.Image)
	if err != nil {
		return Config{}, err
	}
	defaultConfig.Runtime, err = chruntime.Resolve(defaultConfig.Runtime, override.Runtime)
	if err != nil {
		return Config{}, err
	}
	defaultConfig.Metadata, err = metadata.Resolve(defaultConfig.Metadata, override.Metadata)
	if err != nil {
		return Config{}, err
	}
	absolutizePaths(&defaultConfig)

	return defaultConfig, nil
}

func absolutizePaths(cfg *Config) {
	paths := []*string{
		&cfg.SocketPath,
		&cfg.TmpRoot,
		&cfg.ContainerRoot,
	}

	for _, path := range paths {
		if *path == "" {
			continue
		}
		abs, err := filepath.Abs(*path)
		if err != nil {
			panic(fmt.Sprintf("cannot convert config path %q to absolute path: %v", *path, err))
		}
		*path = abs
	}
}

func (c Config) Prepare() error {
	directoryManager := localfs.NewDirectoryManager()

	if err := directoryManager.EnsurePrivateParent(c.SocketPath); err != nil {
		return fmt.Errorf("prepare socket directory: %w", err)
	}
	if err := directoryManager.EnsurePrivateDir(c.TmpRoot); err != nil {
		return fmt.Errorf("prepare tmp root: %w", err)
	}
	if err := directoryManager.EnsurePrivateDir(c.Image.Root); err != nil {
		return fmt.Errorf("prepare image root: %w", err)
	}
	if err := directoryManager.EnsurePrivateDir(c.ContainerRoot); err != nil {
		return fmt.Errorf("prepare container root: %w", err)
	}
	if err := directoryManager.EnsurePrivateDir(c.Runtime.RuntimeRoot); err != nil {
		return fmt.Errorf("prepare runtime root: %w", err)
	}
	if err := directoryManager.EnsurePrivateDir(c.Runtime.RuntimeBinDir); err != nil {
		return fmt.Errorf("prepare runtime bin dir: %w", err)
	}
	if err := directoryManager.EnsurePrivateDir(c.Metadata.Root); err != nil {
		return fmt.Errorf("prepare metadata root: %w", err)
	}
	return nil
}
