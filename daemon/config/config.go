package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type Config struct {
	// API
	HTTPAddr string

	// Storage
	SocketPath string
	TmpRoot    string

	// OCI Bundles
	Bundle chamberBundle.Config

	// Images
	Image chamberImage.Config

	// OCI Runtime
	Runtime chamberRuntime.Config

	// Metadata
	Metadata metadata.Config

	// OpenTelemetry
	OpenTelemetryEndpoint              string
	OpenTelemetryInsecure              bool
	OpenTelemetryTraceSampleRatio      float64
	OpenTelemetryMetricsExportInterval time.Duration

	// Logging
	Logging chamberLogging.Config
}

type Override struct {
	HTTPAddr *string `json:"http_addr,omitempty"`

	SocketPath *string `json:"socket_path,omitempty"`
	TmpRoot    *string `json:"tmp_root,omitempty"`

	Bundle   chamberBundle.Override  `json:"bundle,omitempty"`
	Image    chamberImage.Override   `json:"image,omitempty"`
	Runtime  chamberRuntime.Override `json:"runtime,omitempty"`
	Metadata metadata.Override       `json:"metadata,omitempty"`

	OpenTelemetryEndpoint              *string        `json:"open_telemetry_endpoint,omitempty"`
	OpenTelemetryInsecure              *bool          `json:"open_telemetry_insecure,omitempty"`
	OpenTelemetryTraceSampleRatio      *float64       `json:"open_telemetry_trace_sample_ratio,omitempty"`
	OpenTelemetryMetricsExportInterval *time.Duration `json:"open_telemetry_metrics_export_interval,omitempty"`

	Logging chamberLogging.Override `json:"logging,omitempty"`
}

const (
	defaultHTTPAddr                           = "127.0.0.1:8080"
	defaultOpenTelemetryTraceSampleRatio      = 1.0
	defaultOpenTelemetryMetricsExportInterval = 10 * time.Second
)

func DefaultRoot(getenv func(string) string) string {
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
	rootPath := DefaultRoot(getenv)

	defaultConfig := Config{
		HTTPAddr:   defaultHTTPAddr,
		SocketPath: filepath.Join(rootPath, "run", "chamber.sock"),
		TmpRoot:    filepath.Join(rootPath, "run", "tmp"),

		Bundle:   chamberBundle.DefaultConfig(rootPath),
		Image:    chamberImage.DefaultConfig(rootPath),
		Runtime:  chamberRuntime.DefaultConfig(rootPath),
		Metadata: metadata.DefaultConfig(rootPath),

		OpenTelemetryTraceSampleRatio:      defaultOpenTelemetryTraceSampleRatio,
		OpenTelemetryMetricsExportInterval: defaultOpenTelemetryMetricsExportInterval,
		Logging:                            chamberLogging.DefaultConfig(),
	}

	return Resolve(defaultConfig, override)
}

func LoadFile(path string, override Override, getenv func(string) string) (Config, error) {
	fileOverride, err := ReadOverrideFile(path)
	if err != nil {
		return Config{}, err
	}

	return Load(MergeOverride(fileOverride, override), getenv)
}

func ReadOverrideFile(path string) (Override, error) {
	if path == "" {
		return Override{}, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return Override{}, fmt.Errorf("read config file: %w", err)
	}

	var override Override
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&override); err != nil {
		return Override{}, fmt.Errorf("decode config file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return Override{}, fmt.Errorf("decode config file: config file must contain one JSON object")
	} else if !errors.Is(err, io.EOF) {
		return Override{}, fmt.Errorf("decode config file: %w", err)
	}

	return override, nil
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	var err error
	if override.HTTPAddr != nil {
		defaultConfig.HTTPAddr = *override.HTTPAddr
	}
	if override.SocketPath != nil {
		defaultConfig.SocketPath = *override.SocketPath
	}
	if override.TmpRoot != nil {
		defaultConfig.TmpRoot = *override.TmpRoot
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
	defaultConfig.Logging, err = chamberLogging.Resolve(defaultConfig.Logging, override.Logging)
	if err != nil {
		return Config{}, fmt.Errorf("resolve logging: %w", err)
	}

	defaultConfig.Bundle.Logging = defaultConfig.Logging
	defaultConfig.Image.Logging = defaultConfig.Logging
	defaultConfig.Runtime.Logging = defaultConfig.Logging

	defaultConfig.Bundle, err = chamberBundle.Resolve(defaultConfig.Bundle, override.Bundle)
	if err != nil {
		return Config{}, err
	}
	defaultConfig.Image, err = chamberImage.Resolve(defaultConfig.Image, override.Image)
	if err != nil {
		return Config{}, err
	}
	defaultConfig.Runtime, err = chamberRuntime.Resolve(defaultConfig.Runtime, override.Runtime)
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

func MergeOverride(base Override, overlay Override) Override {
	if overlay.HTTPAddr != nil {
		base.HTTPAddr = overlay.HTTPAddr
	}
	if overlay.SocketPath != nil {
		base.SocketPath = overlay.SocketPath
	}
	if overlay.TmpRoot != nil {
		base.TmpRoot = overlay.TmpRoot
	}
	if overlay.OpenTelemetryEndpoint != nil {
		base.OpenTelemetryEndpoint = overlay.OpenTelemetryEndpoint
	}
	if overlay.OpenTelemetryInsecure != nil {
		base.OpenTelemetryInsecure = overlay.OpenTelemetryInsecure
	}
	if overlay.OpenTelemetryTraceSampleRatio != nil {
		base.OpenTelemetryTraceSampleRatio = overlay.OpenTelemetryTraceSampleRatio
	}
	if overlay.OpenTelemetryMetricsExportInterval != nil {
		base.OpenTelemetryMetricsExportInterval = overlay.OpenTelemetryMetricsExportInterval
	}
	base.Logging = mergeLoggingOverride(base.Logging, overlay.Logging)

	base.Bundle = mergeBundleOverride(base.Bundle, overlay.Bundle)
	base.Image = mergeImageOverride(base.Image, overlay.Image)
	base.Runtime = mergeRuntimeOverride(base.Runtime, overlay.Runtime)
	base.Metadata = mergeMetadataOverride(base.Metadata, overlay.Metadata)
	return base
}

func absolutizePaths(cfg *Config) {
	paths := []*string{
		&cfg.SocketPath,
		&cfg.TmpRoot,
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

func mergeBundleOverride(base chamberBundle.Override, overlay chamberBundle.Override) chamberBundle.Override {
	if overlay.Root != nil {
		base.Root = overlay.Root
	}
	base.Logging = mergeLoggingOverride(base.Logging, overlay.Logging)
	return base
}

func mergeImageOverride(base chamberImage.Override, overlay chamberImage.Override) chamberImage.Override {
	if overlay.Root != nil {
		base.Root = overlay.Root
	}
	base.Logging = mergeLoggingOverride(base.Logging, overlay.Logging)
	return base
}

func mergeMetadataOverride(base metadata.Override, overlay metadata.Override) metadata.Override {
	if overlay.Root != nil {
		base.Root = overlay.Root
	}
	return base
}

func mergeRuntimeOverride(base chamberRuntime.Override, overlay chamberRuntime.Override) chamberRuntime.Override {
	if overlay.RuntimeRoot != nil {
		base.RuntimeRoot = overlay.RuntimeRoot
	}
	if overlay.RuntimeBinDir != nil {
		base.RuntimeBinDir = overlay.RuntimeBinDir
	}
	if overlay.Name != nil {
		base.Name = overlay.Name
	}
	if overlay.Version != nil {
		base.Version = overlay.Version
	}
	if overlay.URL != nil {
		base.URL = overlay.URL
	}
	if overlay.SHA256 != nil {
		base.SHA256 = overlay.SHA256
	}
	base.Logging = mergeLoggingOverride(base.Logging, overlay.Logging)
	return base
}

func mergeLoggingOverride(base chamberLogging.Override, overlay chamberLogging.Override) chamberLogging.Override {
	if overlay.Level != nil {
		base.Level = overlay.Level
	}
	if overlay.Format != nil {
		base.Format = overlay.Format
	}
	return base
}
