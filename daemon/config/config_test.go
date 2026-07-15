package config

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chbundle "github.com/donglin-wang/chamber/pkg/bundle"
	chimage "github.com/donglin-wang/chamber/pkg/image"
	chruntime "github.com/donglin-wang/chamber/pkg/runtime"
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
		case "Bundle":
			wantType = reflect.TypeOf(chbundle.Override{})
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
		case "github.com/donglin-wang/chamber/pkg/image/gocontainerregistry",
			"github.com/donglin-wang/chamber/pkg/bundle/umoci",
			"github.com/donglin-wang/chamber/daemon/metadata/etcd",
			"github.com/donglin-wang/chamber/pkg/runtime/runc",
			"github.com/donglin-wang/chamber/pkg/shared/localfs":
			t.Fatalf("config package must import generic package boundaries and not filesystem setup %q", importPath)
		}
	}
}

func TestProductionCodeDoesNotImportInternalPackages(t *testing.T) {
	repoRoot := filepath.Clean("../..")
	err := filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		for _, importPath := range parseImports(t, path) {
			if strings.HasPrefix(importPath, "github.com/donglin-wang/chamber/internal/") {
				t.Fatalf("production file %s imports internal package %q", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
}

func TestPublicPackagesDoNotImportDaemonPackages(t *testing.T) {
	pkgRoot := filepath.Clean("../../pkg")
	err := filepath.WalkDir(pkgRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		for _, importPath := range parseImports(t, path) {
			if strings.HasPrefix(importPath, "github.com/donglin-wang/chamber/daemon/") {
				t.Fatalf("public package file %s imports daemon package %q", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk public packages: %v", err)
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
		HTTPAddr:   "127.0.0.1:8080",
		SocketPath: filepath.Join(root, "run", "chamber.sock"),
		TmpRoot:    filepath.Join(root, "run", "tmp"),
		Bundle: chbundle.Config{
			Root: filepath.Join(root, "bundles"),
		},
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
	if cfg.Bundle.Root != filepath.Join(root, "bundles") {
		t.Fatalf("Bundle.Root = %q, want %q", cfg.Bundle.Root, filepath.Join(root, "bundles"))
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
		HTTPAddr:   "127.0.0.1:8080",
		SocketPath: "default/run/chamber.sock",
		TmpRoot:    "default/tmp",
		Bundle: chbundle.Config{
			Root: "default/bundles",
		},
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
		HTTPAddr:   ptr("127.0.0.1:9090"),
		SocketPath: ptr("override/run/chamber.sock"),
		TmpRoot:    ptr("override/tmp"),
		Bundle: chbundle.Override{
			Root: ptr("override/bundles"),
		},
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
		HTTPAddr:   "127.0.0.1:9090",
		SocketPath: mustAbs(t, "override/run/chamber.sock"),
		TmpRoot:    mustAbs(t, "override/tmp"),
		Bundle: chbundle.Config{
			Root: mustAbs(t, "override/bundles"),
		},
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
		HTTPAddr:   "127.0.0.1:8080",
		SocketPath: filepath.Join(root, "default", "run", "chamber.sock"),
		TmpRoot:    filepath.Join(root, "default", "tmp"),
		Bundle: chbundle.Config{
			Root: filepath.Join(root, "default", "bundles"),
		},
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

func TestLoadFileAppliesConfigFileThenCommandLineOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chamberd.json")
	content := `{
		"http_addr": "127.0.0.1:9090",
		"tmp_root": "file/tmp",
		"bundle": { "root": "file/bundles" },
		"image": { "root": "file/images" },
		"runtime": {
			"runtime_root": "file/runtime",
			"name": "crun"
		},
		"open_telemetry_metrics_export_interval": 30000000000,
		"log_level": "debug"
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadFile(path, Override{
		HTTPAddr: ptr("127.0.0.1:7070"),
	}, mapGetenv(map[string]string{
		"HOME": t.TempDir(),
	}))
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}

	if cfg.HTTPAddr != "127.0.0.1:7070" {
		t.Fatalf("HTTPAddr = %q, want command-line override", cfg.HTTPAddr)
	}
	if cfg.TmpRoot != mustAbs(t, "file/tmp") {
		t.Fatalf("TmpRoot = %q, want config file value", cfg.TmpRoot)
	}
	if cfg.Bundle.Root != mustAbs(t, "file/bundles") {
		t.Fatalf("Bundle.Root = %q, want config file value", cfg.Bundle.Root)
	}
	if cfg.Image.Root != mustAbs(t, "file/images") {
		t.Fatalf("Image.Root = %q, want config file value", cfg.Image.Root)
	}
	if cfg.Runtime.Name != "crun" {
		t.Fatalf("Runtime.Name = %q, want crun", cfg.Runtime.Name)
	}
	if cfg.OpenTelemetryMetricsExportInterval != 30*time.Second {
		t.Fatalf("OpenTelemetryMetricsExportInterval = %s, want 30s", cfg.OpenTelemetryMetricsExportInterval)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want debug", cfg.LogLevel)
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

func parseImports(t *testing.T, path string) []string {
	t.Helper()

	fileset := token.NewFileSet()
	file, err := parser.ParseFile(fileset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	imports := make([]string, 0, len(file.Imports))
	for _, importSpec := range file.Imports {
		imports = append(imports, strings.Trim(importSpec.Path.Value, `"`))
	}
	return imports
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
