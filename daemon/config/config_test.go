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
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
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
			wantType = reflect.TypeOf(chamberBundle.Override{})
		case "Image":
			wantType = reflect.TypeOf(chamberImage.Override{})
		case "Runtime":
			wantType = reflect.TypeOf(chamberRuntime.Override{})
		case "Metadata":
			wantType = reflect.TypeOf(metadata.Override{})
		case "Logging":
			wantType = reflect.TypeOf(chamberLogging.Override{})
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
		case "github.com/donglin-wang/chamber/pkg/image/puller",
			"github.com/donglin-wang/chamber/pkg/bundle/directory",
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
	defaultLogging := chamberLogging.DefaultConfig()
	want := Config{
		HTTPAddr:   "127.0.0.1:8080",
		SocketPath: filepath.Join(root, "run", "chamber.sock"),
		TmpRoot:    filepath.Join(root, "run", "tmp"),
		Privilege:  capability.Rootless,
		Bundle: chamberBundle.Config{
			Root:      filepath.Join(root, "bundles"),
			Privilege: capability.Rootless,
			Logging:   defaultLogging,
		},
		Image: chamberImage.Config{
			Root:    filepath.Join(root, "images"),
			Logging: defaultLogging,
		},
		Runtime: chamberRuntime.Config{
			RuntimeRoot:   filepath.Join(root, "run", "runtime"),
			RuntimeBinDir: filepath.Join(root, "bin"),
			Privilege:     capability.Rootless,
			Logging:       defaultLogging,
		},
		Metadata: metadata.Config{
			Root: filepath.Join(root, "metadata"),
		},

		OpenTelemetryTraceSampleRatio:      1.0,
		OpenTelemetryMetricsExportInterval: 10 * time.Second,
		Logging:                            defaultLogging,
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
		Privilege:  capability.Rootless,
		Bundle: chamberBundle.Config{
			Root:      "default/bundles",
			Privilege: capability.Rootless,
		},
		Image: chamberImage.Config{
			Root: "default/images",
		},
		Runtime: chamberRuntime.Config{
			RuntimeRoot:   "default/runtime",
			RuntimeBinDir: "default/bin",
			Name:          "default-runtime",
			Version:       "v0.0.1",
			URL:           "https://example.test/default-runtime",
			SHA256:        "default-sha",
			Privilege:     capability.Rootless,
		},
		Metadata: metadata.Config{
			Root: "default/metadata",
		},

		OpenTelemetryEndpoint:              "localhost:4317",
		OpenTelemetryInsecure:              false,
		OpenTelemetryTraceSampleRatio:      0.25,
		OpenTelemetryMetricsExportInterval: time.Second,

		Logging: chamberLogging.Config{
			Level:  "warn",
			Format: "text",
		},
	}
	override := Override{
		HTTPAddr:   ptr("127.0.0.1:9090"),
		SocketPath: ptr("override/run/chamber.sock"),
		TmpRoot:    ptr("override/tmp"),
		Privilege:  ptr(capability.Rootful),
		Bundle: chamberBundle.Override{
			Root: ptr("override/bundles"),
		},
		Image: chamberImage.Override{
			Root: ptr("override/images"),
		},
		Runtime: chamberRuntime.Override{
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

		Logging: chamberLogging.Override{
			Level:  ptr("debug"),
			Format: ptr("text"),
		},
	}

	cfg, err := Resolve(defaultConfig, override)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	want := Config{
		HTTPAddr:   "127.0.0.1:9090",
		SocketPath: mustAbs(t, "override/run/chamber.sock"),
		TmpRoot:    mustAbs(t, "override/tmp"),
		Privilege:  capability.Rootful,
		Bundle: chamberBundle.Config{
			Root:      mustAbs(t, "override/bundles"),
			Privilege: capability.Rootful,
			Logging: chamberLogging.Config{
				Level:  "debug",
				Format: "text",
			},
		},
		Image: chamberImage.Config{
			Root: mustAbs(t, "override/images"),
			Logging: chamberLogging.Config{
				Level:  "debug",
				Format: "text",
			},
		},
		Runtime: chamberRuntime.Config{
			RuntimeRoot:   mustAbs(t, "override/runtime"),
			RuntimeBinDir: mustAbs(t, "override/bin"),
			Name:          "crun",
			Version:       "v1.2.3",
			URL:           "https://example.test/runtime",
			SHA256:        "override-sha",
			Privilege:     capability.Rootful,
			Logging: chamberLogging.Config{
				Level:  "debug",
				Format: "text",
			},
		},
		Metadata: metadata.Config{
			Root: mustAbs(t, "override/metadata"),
		},

		OpenTelemetryEndpoint:              "otel.example.test:4317",
		OpenTelemetryInsecure:              true,
		OpenTelemetryTraceSampleRatio:      0.75,
		OpenTelemetryMetricsExportInterval: 30 * time.Second,

		Logging: chamberLogging.Config{
			Level:  "debug",
			Format: "text",
		},
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
		Privilege:  capability.Rootless,
		Bundle: chamberBundle.Config{
			Root:      filepath.Join(root, "default", "bundles"),
			Privilege: capability.Rootless,
			Logging: chamberLogging.Config{
				Level:  "warn",
				Format: "text",
			},
		},
		Image: chamberImage.Config{
			Root: filepath.Join(root, "default", "images"),
			Logging: chamberLogging.Config{
				Level:  "warn",
				Format: "text",
			},
		},
		Runtime: chamberRuntime.Config{
			RuntimeRoot:   filepath.Join(root, "default", "runtime"),
			RuntimeBinDir: filepath.Join(root, "default", "bin"),
			Name:          "default-runtime",
			Version:       "v0.0.1",
			URL:           "https://example.test/default-runtime",
			SHA256:        "default-sha",
			Privilege:     capability.Rootless,
			Logging: chamberLogging.Config{
				Level:  "warn",
				Format: "text",
			},
		},
		Metadata: metadata.Config{
			Root: filepath.Join(root, "default", "metadata"),
		},

		OpenTelemetryEndpoint:              "localhost:4317",
		OpenTelemetryInsecure:              true,
		OpenTelemetryTraceSampleRatio:      0.25,
		OpenTelemetryMetricsExportInterval: time.Second,

		Logging: chamberLogging.Config{
			Level:  "warn",
			Format: "text",
		},
	}

	cfg, err := Resolve(defaultConfig, Override{})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if !reflect.DeepEqual(cfg, defaultConfig) {
		t.Fatalf("Resolve() config mismatch:\n got: %#v\nwant: %#v", cfg, defaultConfig)
	}
}

func TestResolveProjectsTopLevelPrivilegeToSDKConfigs(t *testing.T) {
	cfg, err := Resolve(Config{
		Bundle: chamberBundle.Config{
			Privilege: capability.Rootless,
		},
		Runtime: chamberRuntime.Config{
			Privilege: capability.Rootless,
		},
	}, Override{
		Privilege: ptr(capability.Rootful),
	})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	if cfg.Privilege != capability.Rootful {
		t.Fatalf("Privilege = %q, want rootful", cfg.Privilege)
	}
	if cfg.Bundle.Privilege != capability.Rootful {
		t.Fatalf("Bundle.Privilege = %q, want daemon privilege rootful", cfg.Bundle.Privilege)
	}
	if cfg.Runtime.Privilege != capability.Rootful {
		t.Fatalf("Runtime.Privilege = %q, want daemon privilege rootful", cfg.Runtime.Privilege)
	}
}

func TestResolveRejectsNestedSDKPrivilegeOverrides(t *testing.T) {
	tests := map[string]Override{
		"bundle": {
			Bundle: chamberBundle.Override{
				Privilege: ptr(capability.Rootful),
			},
		},
		"runtime": {
			Runtime: chamberRuntime.Override{
				Privilege: ptr(capability.Rootful),
			},
		},
	}

	for name, override := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(Config{}, override)
			if err == nil {
				t.Fatal("Resolve() error = nil, want nested privilege override error")
			}
			if !strings.Contains(err.Error(), "top-level privilege") {
				t.Fatalf("Resolve() error = %v, want top-level privilege error", err)
			}
		})
	}
}

func TestLoadFileAppliesConfigFileThenCommandLineOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chamberd.json")
	content := `{
		"http_addr": "127.0.0.1:9090",
		"tmp_root": "file/tmp",
		"privilege": "rootful",
		"bundle": { "root": "file/bundles" },
		"image": { "root": "file/images" },
		"runtime": {
			"runtime_root": "file/runtime",
			"name": "crun"
		},
		"open_telemetry_metrics_export_interval": 30000000000,
		"logging": { "level": "debug" }
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
	if cfg.Privilege != capability.Rootful {
		t.Fatalf("Privilege = %q, want config file value", cfg.Privilege)
	}
	if cfg.Bundle.Root != mustAbs(t, "file/bundles") {
		t.Fatalf("Bundle.Root = %q, want config file value", cfg.Bundle.Root)
	}
	if cfg.Bundle.Privilege != capability.Rootful {
		t.Fatalf("Bundle.Privilege = %q, want top-level daemon privilege", cfg.Bundle.Privilege)
	}
	if cfg.Image.Root != mustAbs(t, "file/images") {
		t.Fatalf("Image.Root = %q, want config file value", cfg.Image.Root)
	}
	if cfg.Runtime.Name != "crun" {
		t.Fatalf("Runtime.Name = %q, want crun", cfg.Runtime.Name)
	}
	if cfg.Runtime.Privilege != capability.Rootful {
		t.Fatalf("Runtime.Privilege = %q, want top-level daemon privilege", cfg.Runtime.Privilege)
	}
	if cfg.OpenTelemetryMetricsExportInterval != 30*time.Second {
		t.Fatalf("OpenTelemetryMetricsExportInterval = %s, want 30s", cfg.OpenTelemetryMetricsExportInterval)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("Logging.Level = %q, want debug", cfg.Logging.Level)
	}
}

func TestMergeOverrideAppliesPrivilegeOverlays(t *testing.T) {
	base := Override{
		Privilege: ptr(capability.Rootless),
		Bundle: chamberBundle.Override{
			Privilege: ptr(capability.Rootless),
		},
		Runtime: chamberRuntime.Override{
			Privilege: ptr(capability.Rootless),
		},
	}
	overlay := Override{
		Privilege: ptr(capability.Rootful),
		Bundle: chamberBundle.Override{
			Privilege: ptr(capability.Rootful),
		},
		Runtime: chamberRuntime.Override{
			Privilege: ptr(capability.Rootful),
		},
	}

	merged := MergeOverride(base, overlay)

	if merged.Privilege == nil || *merged.Privilege != capability.Rootful {
		t.Fatalf("Privilege = %v, want rootful", merged.Privilege)
	}
	if merged.Bundle.Privilege == nil || *merged.Bundle.Privilege != capability.Rootful {
		t.Fatalf("Bundle.Privilege = %v, want rootful", merged.Bundle.Privilege)
	}
	if merged.Runtime.Privilege == nil || *merged.Runtime.Privilege != capability.Rootful {
		t.Fatalf("Runtime.Privilege = %v, want rootful", merged.Runtime.Privilege)
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
