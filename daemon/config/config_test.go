package config

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/donglin-wang/chamber/internal/metadata"
	chruntime "github.com/donglin-wang/chamber/internal/runtime"
)

func TestOverrideFieldsMatchConfigFields(t *testing.T) {
	configType := reflect.TypeOf(Config{})
	overrideType := reflect.TypeOf(Override{})

	configFields := fieldsByName(configType)
	overrideFields := fieldsByName(overrideType)

	for name, configField := range configFields {
		overrideField, ok := overrideFields[name]
		if !ok {
			t.Fatalf("Override is missing field %s", name)
		}

		wantType := reflect.PointerTo(configField.Type)
		switch name {
		case "Image":
			wantType = reflect.TypeOf(chimage.Override{})
		case "Runtime":
			wantType = reflect.TypeOf(chruntime.Override{})
		case "Metadata":
			wantType = reflect.TypeOf(metadata.Override{})
		}
		if overrideField.Type != wantType {
			t.Fatalf("Override.%s has type %s, want %s", name, overrideField.Type, wantType)
		}
	}

	for name := range overrideFields {
		if _, ok := configFields[name]; !ok {
			t.Fatalf("Override has extra field %s", name)
		}
	}
}

func TestConfigDoesNotImportConcreteImplementations(t *testing.T) {
	fileset := token.NewFileSet()
	file, err := parser.ParseFile(fileset, "config.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
	}

	for _, importSpec := range file.Imports {
		importPath := strings.Trim(importSpec.Path.Value, `"`)
		switch importPath {
		case "github.com/donglin-wang/chamber/internal/image/gocontainerregistry",
			"github.com/donglin-wang/chamber/internal/metadata/etcd",
			"github.com/donglin-wang/chamber/internal/runtime/runc",
			"github.com/donglin-wang/chamber/internal/shared/localfs":
			t.Fatalf("config package must import generic package boundaries and not filesystem setup %q", importPath)
		}
	}
}

func TestLoadDerivesDefaultPathsFromXDGDataHome(t *testing.T) {
	xdgDataHome := filepath.Join(t.TempDir(), "xdg-data")

	cfg, err := Load(Override{}, mapGetenv(map[string]string{
		"XDG_DATA_HOME": xdgDataHome,
		"HOME":          filepath.Join(t.TempDir(), "home"),
	}))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	root := filepath.Join(xdgDataHome, "chamber")
	want := Config{
		SocketPath:    filepath.Join(root, "run", "chamber.sock"),
		TmpRoot:       filepath.Join(root, "run", "tmp"),
		ContainerRoot: filepath.Join(root, "containers"),
		Image: chimage.Config{
			Root: filepath.Join(root, "images"),
		},
		Runtime: chruntime.Config{
			RuntimeRoot:   filepath.Join(root, "run", "runtime"),
			RuntimeBinDir: filepath.Join(root, "bin"),
			Name:          "runc",
		},
		Metadata: metadata.Config{
			Root: filepath.Join(root, "metadata"),
		},

		OpenTelemetryTraceSampleRatio:      1.0,
		OpenTelemetryMetricsExportInterval: 10 * time.Second,
		LogLevel:                           "info",
		LogFormat:                          "json",
	}

	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load() config mismatch:\n got: %#v\nwant: %#v", cfg, want)
	}
}

func TestLoadFallsBackToHomeWhenXDGDataHomeIsUnset(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")

	cfg, err := Load(Override{}, mapGetenv(map[string]string{
		"HOME": home,
	}))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	root := filepath.Join(home, ".local", "share", "chamber")
	if cfg.Image.Root != filepath.Join(root, "images") {
		t.Fatalf("Image.Root = %q, want %q", cfg.Image.Root, filepath.Join(root, "images"))
	}
	if cfg.SocketPath != filepath.Join(root, "run", "chamber.sock") {
		t.Fatalf("SocketPath = %q, want %q", cfg.SocketPath, filepath.Join(root, "run", "chamber.sock"))
	}
	if cfg.Runtime.RuntimeRoot != filepath.Join(root, "run", "runtime") {
		t.Fatalf("Runtime.RuntimeRoot = %q, want %q", cfg.Runtime.RuntimeRoot, filepath.Join(root, "run", "runtime"))
	}
	if cfg.Metadata.Root != filepath.Join(root, "metadata") {
		t.Fatalf("Metadata.Root = %q, want %q", cfg.Metadata.Root, filepath.Join(root, "metadata"))
	}
}

func TestLoadPanicsWhenRootPathCannotBeDerived(t *testing.T) {
	defer func() {
		err := recover()
		if err == nil {
			t.Fatal("Load did not panic")
		}
		if !strings.Contains(err.(string), "neither $XDG_DATA_HOME nor $HOME are set") {
			t.Fatalf("panic = %v, want missing environment explanation", err)
		}
	}()

	_, _ = Load(Override{}, mapGetenv(nil))
}

func TestResolveAppliesOverridesAndAbsolutizesPaths(t *testing.T) {
	defaultConfig := Config{
		SocketPath:    "default/run/chamber.sock",
		TmpRoot:       "default/tmp",
		ContainerRoot: "default/containers",
		Image: chimage.Config{
			Root: "default/images",
		},
		Runtime: chruntime.Config{
			RuntimeRoot:   "default/runtime",
			RuntimeBinDir: "default/bin",
			Name:          "default-runtime",
			Version:       "v0.0.1",
			URL:           "https://example.test/default-runtime",
			SHA256:        "default-sha",
		},
		Metadata: metadata.Config{
			Root: "default/metadata",
		},

		OpenTelemetryEndpoint:              "localhost:4317",
		OpenTelemetryInsecure:              false,
		OpenTelemetryTraceSampleRatio:      0.25,
		OpenTelemetryMetricsExportInterval: time.Second,

		LogLevel:  "warn",
		LogFormat: "text",
	}
	override := Override{
		SocketPath:    ptr("override/run/chamber.sock"),
		TmpRoot:       ptr("override/tmp"),
		ContainerRoot: ptr("override/containers"),
		Image: chimage.Override{
			Root: ptr("override/images"),
		},
		Runtime: chruntime.Override{
			RuntimeRoot:   ptr("override/runtime"),
			RuntimeBinDir: ptr("override/bin"),
			Name:          ptr("crun"),
			Version:       ptr("v1.2.3"),
			URL:           ptr("https://example.test/runtime"),
			SHA256:        ptr("override-sha"),
		},
		Metadata: metadata.Override{
			Root: ptr("override/metadata"),
		},

		OpenTelemetryEndpoint:              ptr("otel.example.test:4317"),
		OpenTelemetryInsecure:              ptr(true),
		OpenTelemetryTraceSampleRatio:      ptr(0.75),
		OpenTelemetryMetricsExportInterval: ptr(30 * time.Second),

		LogLevel:  ptr("debug"),
		LogFormat: ptr("console"),
	}

	cfg, err := Resolve(defaultConfig, override)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	want := Config{
		SocketPath:    mustAbs(t, "override/run/chamber.sock"),
		TmpRoot:       mustAbs(t, "override/tmp"),
		ContainerRoot: mustAbs(t, "override/containers"),
		Image: chimage.Config{
			Root: mustAbs(t, "override/images"),
		},
		Runtime: chruntime.Config{
			RuntimeRoot:   mustAbs(t, "override/runtime"),
			RuntimeBinDir: mustAbs(t, "override/bin"),
			Name:          "crun",
			Version:       "v1.2.3",
			URL:           "https://example.test/runtime",
			SHA256:        "override-sha",
		},
		Metadata: metadata.Config{
			Root: mustAbs(t, "override/metadata"),
		},

		OpenTelemetryEndpoint:              "otel.example.test:4317",
		OpenTelemetryInsecure:              true,
		OpenTelemetryTraceSampleRatio:      0.75,
		OpenTelemetryMetricsExportInterval: 30 * time.Second,

		LogLevel:  "debug",
		LogFormat: "console",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Resolve() config mismatch:\n got: %#v\nwant: %#v", cfg, want)
	}
}

func TestResolveLeavesDefaultsWhenOverrideFieldsAreNil(t *testing.T) {
	root := t.TempDir()
	defaultConfig := Config{
		SocketPath:    filepath.Join(root, "default", "run", "chamber.sock"),
		TmpRoot:       filepath.Join(root, "default", "tmp"),
		ContainerRoot: filepath.Join(root, "default", "containers"),
		Image: chimage.Config{
			Root: filepath.Join(root, "default", "images"),
		},
		Runtime: chruntime.Config{
			RuntimeRoot:   filepath.Join(root, "default", "runtime"),
			RuntimeBinDir: filepath.Join(root, "default", "bin"),
			Name:          "default-runtime",
			Version:       "v0.0.1",
			URL:           "https://example.test/default-runtime",
			SHA256:        "default-sha",
		},
		Metadata: metadata.Config{
			Root: filepath.Join(root, "default", "metadata"),
		},

		OpenTelemetryEndpoint:              "localhost:4317",
		OpenTelemetryInsecure:              true,
		OpenTelemetryTraceSampleRatio:      0.25,
		OpenTelemetryMetricsExportInterval: time.Second,

		LogLevel:  "warn",
		LogFormat: "text",
	}

	cfg, err := Resolve(defaultConfig, Override{})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if !reflect.DeepEqual(cfg, defaultConfig) {
		t.Fatalf("Resolve() config mismatch:\n got: %#v\nwant: %#v", cfg, defaultConfig)
	}
}

func fieldsByName(structType reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		fields[field.Name] = field
	}
	return fields
}

func mapGetenv(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func ptr[T any](value T) *T {
	return &value
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()

	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q) returned error: %v", path, err)
	}
	return abs
}
