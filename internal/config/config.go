package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	// Storage
	SocketPath    string
	TmpRoot       string
	ImageRoot     string
	ContainerRoot string
	RuntimeRoot   string
	RuntimeBinDir string
	MetadataRoot  string

	// OCI Runtime
	RuntimeName    string
	RuntimeVersion string
	RuntimeURL     string
	RuntimeSHA256  string

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
	ImageRoot     *string
	ContainerRoot *string
	RuntimeRoot   *string
	RuntimeBinDir *string
	MetadataRoot  *string

	RuntimeName    *string
	RuntimeVersion *string
	RuntimeURL     *string
	RuntimeSHA256  *string

	OpenTelemetryEndpoint              *string
	OpenTelemetryInsecure              *bool
	OpenTelemetryTraceSampleRatio      *float64
	OpenTelemetryMetricsExportInterval *time.Duration

	LogLevel  *string
	LogFormat *string
}

const (
	defaultRuntimeName                        = "runc"
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
		ImageRoot:     filepath.Join(rootPath, "images"),
		ContainerRoot: filepath.Join(rootPath, "containers"),
		RuntimeRoot:   filepath.Join(rootPath, "run", "runtime"),
		RuntimeBinDir: filepath.Join(rootPath, "bin"),
		MetadataRoot:  filepath.Join(rootPath, "metadata", "etcd"),

		RuntimeName: defaultRuntimeName,

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
	if override.ImageRoot != nil {
		defaultConfig.ImageRoot = *override.ImageRoot
	}
	if override.ContainerRoot != nil {
		defaultConfig.ContainerRoot = *override.ContainerRoot
	}
	if override.RuntimeRoot != nil {
		defaultConfig.RuntimeRoot = *override.RuntimeRoot
	}
	if override.RuntimeBinDir != nil {
		defaultConfig.RuntimeBinDir = *override.RuntimeBinDir
	}
	if override.MetadataRoot != nil {
		defaultConfig.MetadataRoot = *override.MetadataRoot
	}
	if override.RuntimeName != nil {
		defaultConfig.RuntimeName = *override.RuntimeName
	}
	if override.RuntimeVersion != nil {
		defaultConfig.RuntimeVersion = *override.RuntimeVersion
	}
	if override.RuntimeURL != nil {
		defaultConfig.RuntimeURL = *override.RuntimeURL
	}
	if override.RuntimeSHA256 != nil {
		defaultConfig.RuntimeSHA256 = *override.RuntimeSHA256
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

	absolutizePaths(&defaultConfig)

	return defaultConfig, nil
}

func absolutizePaths(cfg *Config) {
	paths := []*string{
		&cfg.SocketPath,
		&cfg.TmpRoot,
		&cfg.ImageRoot,
		&cfg.ContainerRoot,
		&cfg.RuntimeRoot,
		&cfg.RuntimeBinDir,
		&cfg.MetadataRoot,
	}

	for _, path := range paths {
		abs, err := filepath.Abs(*path)
		if err != nil {
			panic(fmt.Sprintf("cannot convert config path %q to absolute path: %v", *path, err))
		}
		*path = abs
	}
}

func (c Config) Prepare() error {
	if err := ensurePath(filepath.Dir(c.SocketPath)); err != nil {
		return fmt.Errorf("prepare socket directory: %w", err)
	}
	if err := ensurePath(c.TmpRoot); err != nil {
		return fmt.Errorf("prepare tmp root: %w", err)
	}
	if err := ensurePath(c.ImageRoot); err != nil {
		return fmt.Errorf("prepare image root: %w", err)
	}
	if err := ensurePath(c.ContainerRoot); err != nil {
		return fmt.Errorf("prepare container root: %w", err)
	}
	if err := ensurePath(c.RuntimeRoot); err != nil {
		return fmt.Errorf("prepare runtime root: %w", err)
	}
	if err := ensurePath(c.RuntimeBinDir); err != nil {
		return fmt.Errorf("prepare runtime bin dir: %w", err)
	}
	if err := ensurePath(c.MetadataRoot); err != nil {
		return fmt.Errorf("prepare metadata root: %w", err)
	}
	return nil
}

func ensurePath(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("error creating path %q: %w", path, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("error reading metadata for path %q: %w", path, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}

	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("path %q must not be readable, writable, or executable by group or other users", path)
	}
	return nil
}
