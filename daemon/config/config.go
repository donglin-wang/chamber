package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type Config struct {
	// API
	HTTPAddr string

	// Storage
	SocketPath string
	TmpRoot    string

	// Privilege
	Privilege capability.Privilege

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

type Input struct {
	HTTPAddr *string `json:"http_addr,omitempty"`

	SocketPath *string `json:"socket_path,omitempty"`
	TmpRoot    *string `json:"tmp_root,omitempty"`

	Privilege *capability.Privilege `json:"privilege,omitempty"`

	Bundle   bundleInput   `json:"bundle,omitempty"`
	Image    imageInput    `json:"image,omitempty"`
	Runtime  runtimeInput  `json:"runtime,omitempty"`
	Metadata metadataInput `json:"metadata,omitempty"`

	OpenTelemetryEndpoint              *string        `json:"open_telemetry_endpoint,omitempty"`
	OpenTelemetryInsecure              *bool          `json:"open_telemetry_insecure,omitempty"`
	OpenTelemetryTraceSampleRatio      *float64       `json:"open_telemetry_trace_sample_ratio,omitempty"`
	OpenTelemetryMetricsExportInterval *time.Duration `json:"open_telemetry_metrics_export_interval,omitempty"`

	Logging loggingInput `json:"logging,omitempty"`
}

type bundleInput struct {
	Root      *string               `json:"root,omitempty"`
	Privilege *capability.Privilege `json:"privilege,omitempty"`
	Logging   loggingInput          `json:"logging,omitempty"`
}

type imageInput struct {
	Root    *string      `json:"root,omitempty"`
	Logging loggingInput `json:"logging,omitempty"`
}

type runtimeInput struct {
	RuntimeRoot   *string               `json:"runtime_root,omitempty"`
	RuntimeBinDir *string               `json:"runtime_bin_dir,omitempty"`
	Name          *string               `json:"name,omitempty"`
	Privilege     *capability.Privilege `json:"privilege,omitempty"`
	Logging       loggingInput          `json:"logging,omitempty"`
}

type metadataInput struct {
	Root *string `json:"root,omitempty"`
}

type loggingInput struct {
	Level  *string `json:"level,omitempty"`
	Format *string `json:"format,omitempty"`
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

func Load(input Input, getenv func(string) string) (Config, error) {
	rootPath := DefaultRoot(getenv)

	defaultConfig := Config{
		HTTPAddr:   defaultHTTPAddr,
		SocketPath: filepath.Join(rootPath, "run", "chamber.sock"),
		TmpRoot:    filepath.Join(rootPath, "run", "tmp"),
		Privilege:  capability.Rootless,

		Bundle:   chamberBundle.DefaultConfig(rootPath),
		Image:    chamberImage.DefaultConfig(rootPath),
		Runtime:  chamberRuntime.DefaultConfig(rootPath),
		Metadata: metadata.DefaultConfig(rootPath),

		OpenTelemetryTraceSampleRatio:      defaultOpenTelemetryTraceSampleRatio,
		OpenTelemetryMetricsExportInterval: defaultOpenTelemetryMetricsExportInterval,
		Logging:                            chamberLogging.DefaultConfig(),
	}

	return ApplyInput(defaultConfig, input)
}

func LoadFile(path string, commandLineInput Input, getenv func(string) string) (Config, error) {
	fileInput, err := ReadInputFile(path)
	if err != nil {
		return Config{}, err
	}

	return Load(MergeInput(fileInput, commandLineInput), getenv)
}

func ReadInputFile(path string) (Input, error) {
	if path == "" {
		return Input{}, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return Input{}, fmt.Errorf("read config file: %w", err)
	}

	var input Input
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Input{}, fmt.Errorf("decode config file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return Input{}, fmt.Errorf("decode config file: config file must contain one JSON object")
	} else if !errors.Is(err, io.EOF) {
		return Input{}, fmt.Errorf("decode config file: %w", err)
	}

	return input, nil
}

func ApplyInput(defaultConfig Config, input Input) (Config, error) {
	if input.HTTPAddr != nil {
		defaultConfig.HTTPAddr = *input.HTTPAddr
	}
	if input.SocketPath != nil {
		defaultConfig.SocketPath = *input.SocketPath
	}
	if input.TmpRoot != nil {
		defaultConfig.TmpRoot = *input.TmpRoot
	}
	if input.Privilege != nil {
		defaultConfig.Privilege = *input.Privilege
	}
	if input.OpenTelemetryEndpoint != nil {
		defaultConfig.OpenTelemetryEndpoint = *input.OpenTelemetryEndpoint
	}
	if input.OpenTelemetryInsecure != nil {
		defaultConfig.OpenTelemetryInsecure = *input.OpenTelemetryInsecure
	}
	if input.OpenTelemetryTraceSampleRatio != nil {
		defaultConfig.OpenTelemetryTraceSampleRatio = *input.OpenTelemetryTraceSampleRatio
	}
	if input.OpenTelemetryMetricsExportInterval != nil {
		defaultConfig.OpenTelemetryMetricsExportInterval = *input.OpenTelemetryMetricsExportInterval
	}
	if input.Bundle.Privilege != nil {
		return Config{}, fmt.Errorf("bundle privilege must be configured with top-level privilege")
	}
	if input.Runtime.Privilege != nil {
		return Config{}, fmt.Errorf("runtime privilege must be configured with top-level privilege")
	}
	defaultConfig.Logging = applyLoggingInput(defaultConfig.Logging, input.Logging)
	if err := validateLogging(defaultConfig.Logging); err != nil {
		return Config{}, fmt.Errorf("validate logging: %w", err)
	}

	defaultConfig.Bundle.Logging = defaultConfig.Logging
	defaultConfig.Image.Logging = defaultConfig.Logging
	defaultConfig.Runtime.Logging = defaultConfig.Logging

	applyBundleInput(&defaultConfig.Bundle, input.Bundle)
	applyImageInput(&defaultConfig.Image, input.Image)
	applyRuntimeInput(&defaultConfig.Runtime, input.Runtime)
	applyMetadataInput(&defaultConfig.Metadata, input.Metadata)
	if defaultConfig.Privilege == "" {
		defaultConfig.Privilege = capability.Rootless
	}
	defaultConfig.Bundle.Privilege = defaultConfig.Privilege
	defaultConfig.Runtime.Privilege = defaultConfig.Privilege
	if defaultConfig.Runtime.Name != "" && !chamberRuntime.IsSupportedName(defaultConfig.Runtime.Name) {
		return Config{}, fmt.Errorf("unsupported runtime name %q (supported: %s)", defaultConfig.Runtime.Name, strings.Join(chamberRuntime.SupportedNames(), ", "))
	}
	if err := validateLogging(defaultConfig.Bundle.Logging); err != nil {
		return Config{}, fmt.Errorf("validate bundle logging: %w", err)
	}
	if err := validateLogging(defaultConfig.Image.Logging); err != nil {
		return Config{}, fmt.Errorf("validate image logging: %w", err)
	}
	if err := validateLogging(defaultConfig.Runtime.Logging); err != nil {
		return Config{}, fmt.Errorf("validate runtime logging: %w", err)
	}
	absolutizePaths(&defaultConfig)

	return defaultConfig, nil
}

func MergeInput(base Input, overlay Input) Input {
	if overlay.HTTPAddr != nil {
		base.HTTPAddr = overlay.HTTPAddr
	}
	if overlay.SocketPath != nil {
		base.SocketPath = overlay.SocketPath
	}
	if overlay.TmpRoot != nil {
		base.TmpRoot = overlay.TmpRoot
	}
	if overlay.Privilege != nil {
		base.Privilege = overlay.Privilege
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
	base.Logging = mergeLoggingInput(base.Logging, overlay.Logging)

	base.Bundle = mergeBundleInput(base.Bundle, overlay.Bundle)
	base.Image = mergeImageInput(base.Image, overlay.Image)
	base.Runtime = mergeRuntimeInput(base.Runtime, overlay.Runtime)
	base.Metadata = mergeMetadataInput(base.Metadata, overlay.Metadata)
	return base
}

func absolutizePaths(cfg *Config) {
	paths := []*string{
		&cfg.SocketPath,
		&cfg.TmpRoot,
		&cfg.Bundle.Root,
		&cfg.Image.Root,
		&cfg.Runtime.RuntimeRoot,
		&cfg.Runtime.RuntimeBinDir,
		&cfg.Metadata.Root,
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

func mergeBundleInput(base bundleInput, overlay bundleInput) bundleInput {
	if overlay.Root != nil {
		base.Root = overlay.Root
	}
	if overlay.Privilege != nil {
		base.Privilege = overlay.Privilege
	}
	base.Logging = mergeLoggingInput(base.Logging, overlay.Logging)
	return base
}

func mergeImageInput(base imageInput, overlay imageInput) imageInput {
	if overlay.Root != nil {
		base.Root = overlay.Root
	}
	base.Logging = mergeLoggingInput(base.Logging, overlay.Logging)
	return base
}

func mergeMetadataInput(base metadataInput, overlay metadataInput) metadataInput {
	if overlay.Root != nil {
		base.Root = overlay.Root
	}
	return base
}

func mergeRuntimeInput(base runtimeInput, overlay runtimeInput) runtimeInput {
	if overlay.RuntimeRoot != nil {
		base.RuntimeRoot = overlay.RuntimeRoot
	}
	if overlay.RuntimeBinDir != nil {
		base.RuntimeBinDir = overlay.RuntimeBinDir
	}
	if overlay.Name != nil {
		base.Name = overlay.Name
	}
	if overlay.Privilege != nil {
		base.Privilege = overlay.Privilege
	}
	base.Logging = mergeLoggingInput(base.Logging, overlay.Logging)
	return base
}

func mergeLoggingInput(base loggingInput, overlay loggingInput) loggingInput {
	if overlay.Level != nil {
		base.Level = overlay.Level
	}
	if overlay.Format != nil {
		base.Format = overlay.Format
	}
	return base
}

func applyBundleInput(config *chamberBundle.Config, input bundleInput) {
	if input.Root != nil {
		config.Root = *input.Root
	}
	config.Logging = applyLoggingInput(config.Logging, input.Logging)
}

func applyImageInput(config *chamberImage.Config, input imageInput) {
	if input.Root != nil {
		config.Root = *input.Root
	}
	config.Logging = applyLoggingInput(config.Logging, input.Logging)
}

func applyRuntimeInput(config *chamberRuntime.Config, input runtimeInput) {
	if input.RuntimeRoot != nil {
		config.RuntimeRoot = *input.RuntimeRoot
	}
	if input.RuntimeBinDir != nil {
		config.RuntimeBinDir = *input.RuntimeBinDir
	}
	if input.Name != nil {
		config.Name = *input.Name
	}
	config.Logging = applyLoggingInput(config.Logging, input.Logging)
}

func applyMetadataInput(config *metadata.Config, input metadataInput) {
	if input.Root != nil {
		config.Root = *input.Root
	}
}

func applyLoggingInput(config chamberLogging.Config, input loggingInput) chamberLogging.Config {
	if input.Level != nil {
		config.Level = *input.Level
	}
	if input.Format != nil {
		config.Format = *input.Format
	}
	return config
}

func validateLogging(config chamberLogging.Config) error {
	_, err := chamberLogging.NewLogger(io.Discard, config)
	return err
}
