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
	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	chamberImageShared "github.com/donglin-wang/chamber/pkg/image/shared"
	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

func TestInputFieldsMatchConfigFields(t *testing.T) {
	configType := reflect.TypeOf(Config{})
	inputType := reflect.TypeOf(Input{})

	configFields := fieldsByName(configType)
	inputFields := fieldsByName(inputType)

	for name, configField := range configFields {
		inputField, ok := inputFields[name]
		if !ok {
			t.Fatalf("Input is missing field %s", name)
		}

		wantType := reflect.PointerTo(configField.Type)
		switch name {
		case "Bundle":
			wantType = reflect.TypeOf(bundleInput{})
		case "Image":
			wantType = reflect.TypeOf(imageInput{})
		case "Runtime":
			wantType = reflect.TypeOf(runtimeInput{})
		case "Metadata":
			wantType = reflect.TypeOf(metadataInput{})
		case "Logging":
			wantType = reflect.TypeOf(loggingInput{})
		}
		if inputField.Type != wantType {
			t.Fatalf("Input.%s has type %s, want %s", name, inputField.Type, wantType)
		}
	}

	for name := range inputFields {
		if _, ok := configFields[name]; !ok {
			t.Fatalf("Input has extra field %s", name)
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

	cfg, err := Load(Input{}, mapGetenv(map[string]string{
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
		Bundle: chamberBundleShared.Config{
			Root:      filepath.Join(root, "bundles"),
			Name:      chamberBundleShared.ProvisionerNameDirectory,
			Privilege: capability.Rootless,
			Logging:   defaultLogging,
		},
		Image: chamberImageShared.Config{
			Root:    filepath.Join(root, "images"),
			Logging: defaultLogging,
		},
		Runtime: chamberRuntimeShared.Config{
			RuntimeRoot:   filepath.Join(root, "run", "runtime"),
			RuntimeBinDir: filepath.Join(root, "bin"),
			Name:          chamberRuntimeShared.RuntimeNameRunc,
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

	cfg, err := Load(Input{}, mapGetenv(map[string]string{
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

	_, _ = Load(Input{}, mapGetenv(nil))
}

func TestApplyInputAppliesInputsAndAbsolutizesPaths(t *testing.T) {
	defaultConfig := Config{
		HTTPAddr:   "127.0.0.1:8080",
		SocketPath: "default/run/chamber.sock",
		TmpRoot:    "default/tmp",
		Privilege:  capability.Rootless,
		Bundle: chamberBundleShared.Config{
			Root:      "default/bundles",
			Name:      chamberBundleShared.ProvisionerNameDirectory,
			Privilege: capability.Rootless,
		},
		Image: chamberImageShared.Config{
			Root: "default/images",
		},
		Runtime: chamberRuntimeShared.Config{
			RuntimeRoot:   "default/runtime",
			RuntimeBinDir: "default/bin",
			Name:          chamberRuntimeShared.RuntimeNameRunc,
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
	input := Input{
		HTTPAddr:   ptr("127.0.0.1:9090"),
		SocketPath: ptr("input/run/chamber.sock"),
		TmpRoot:    ptr("input/tmp"),
		Privilege:  ptr(capability.Rootful),
		Bundle: bundleInput{
			Root: ptr("input/bundles"),
		},
		Image: imageInput{
			Root: ptr("input/images"),
		},
		Runtime: runtimeInput{
			RuntimeRoot:   ptr("input/runtime"),
			RuntimeBinDir: ptr("input/bin"),
			Name:          ptr(chamberRuntimeShared.RuntimeNameRunc),
		},
		Metadata: metadataInput{
			Root: ptr("input/metadata"),
		},

		OpenTelemetryEndpoint:              ptr("otel.example.test:4317"),
		OpenTelemetryInsecure:              ptr(true),
		OpenTelemetryTraceSampleRatio:      ptr(0.75),
		OpenTelemetryMetricsExportInterval: ptr(30 * time.Second),

		Logging: loggingInput{
			Level:  ptr("debug"),
			Format: ptr("text"),
		},
	}

	cfg, err := ApplyInput(defaultConfig, input)
	if err != nil {
		t.Fatalf("ApplyInput returned error: %v", err)
	}

	want := Config{
		HTTPAddr:   "127.0.0.1:9090",
		SocketPath: mustAbs(t, "input/run/chamber.sock"),
		TmpRoot:    mustAbs(t, "input/tmp"),
		Privilege:  capability.Rootful,
		Bundle: chamberBundleShared.Config{
			Root:      mustAbs(t, "input/bundles"),
			Name:      chamberBundleShared.ProvisionerNameDirectory,
			Privilege: capability.Rootful,
			Logging: chamberLogging.Config{
				Level:  "debug",
				Format: "text",
			},
		},
		Image: chamberImageShared.Config{
			Root: mustAbs(t, "input/images"),
			Logging: chamberLogging.Config{
				Level:  "debug",
				Format: "text",
			},
		},
		Runtime: chamberRuntimeShared.Config{
			RuntimeRoot:   mustAbs(t, "input/runtime"),
			RuntimeBinDir: mustAbs(t, "input/bin"),
			Name:          chamberRuntimeShared.RuntimeNameRunc,
			Privilege:     capability.Rootful,
			Logging: chamberLogging.Config{
				Level:  "debug",
				Format: "text",
			},
		},
		Metadata: metadata.Config{
			Root: mustAbs(t, "input/metadata"),
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
		t.Fatalf("ApplyInput() config mismatch:\n got: %#v\nwant: %#v", cfg, want)
	}
}

func TestApplyInputLeavesDefaultsWhenInputFieldsAreNil(t *testing.T) {
	root := t.TempDir()
	defaultConfig := Config{
		HTTPAddr:   "127.0.0.1:8080",
		SocketPath: filepath.Join(root, "default", "run", "chamber.sock"),
		TmpRoot:    filepath.Join(root, "default", "tmp"),
		Privilege:  capability.Rootless,
		Bundle: chamberBundleShared.Config{
			Root:      filepath.Join(root, "default", "bundles"),
			Name:      chamberBundleShared.ProvisionerNameDirectory,
			Privilege: capability.Rootless,
			Logging: chamberLogging.Config{
				Level:  "warn",
				Format: "text",
			},
		},
		Image: chamberImageShared.Config{
			Root: filepath.Join(root, "default", "images"),
			Logging: chamberLogging.Config{
				Level:  "warn",
				Format: "text",
			},
		},
		Runtime: chamberRuntimeShared.Config{
			RuntimeRoot:   filepath.Join(root, "default", "runtime"),
			RuntimeBinDir: filepath.Join(root, "default", "bin"),
			Name:          chamberRuntimeShared.RuntimeNameRunc,
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

	cfg, err := ApplyInput(defaultConfig, Input{})
	if err != nil {
		t.Fatalf("ApplyInput returned error: %v", err)
	}

	if !reflect.DeepEqual(cfg, defaultConfig) {
		t.Fatalf("ApplyInput() config mismatch:\n got: %#v\nwant: %#v", cfg, defaultConfig)
	}
}

func TestApplyInputProjectsTopLevelPrivilegeToSDKConfigs(t *testing.T) {
	cfg, err := ApplyInput(Config{
		Bundle: chamberBundleShared.Config{
			Name:      chamberBundleShared.ProvisionerNameDirectory,
			Privilege: capability.Rootless,
		},
		Runtime: chamberRuntimeShared.Config{
			Privilege: capability.Rootless,
		},
	}, Input{
		Privilege: ptr(capability.Rootful),
	})
	if err != nil {
		t.Fatalf("ApplyInput returned error: %v", err)
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

func TestApplyInputRejectsNestedSDKPrivilegeInputs(t *testing.T) {
	tests := map[string]Input{
		"bundle": {
			Bundle: bundleInput{
				Privilege: ptr(capability.Rootful),
			},
		},
		"runtime": {
			Runtime: runtimeInput{
				Privilege: ptr(capability.Rootful),
			},
		},
	}

	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ApplyInput(Config{}, input)
			if err == nil {
				t.Fatal("ApplyInput() error = nil, want nested privilege input error")
			}
			if !strings.Contains(err.Error(), "top-level privilege") {
				t.Fatalf("ApplyInput() error = %v, want top-level privilege error", err)
			}
		})
	}
}

func TestApplyInputRejectsUnsupportedBundleProvisionerName(t *testing.T) {
	_, err := ApplyInput(Config{
		Bundle: chamberBundleShared.Config{
			Name:      "overlay",
			Privilege: capability.Rootless,
		},
		Runtime: chamberRuntimeShared.Config{
			Name:      chamberRuntimeShared.RuntimeNameRunc,
			Privilege: capability.Rootless,
		},
	}, Input{})
	if err == nil {
		t.Fatal("ApplyInput() error = nil, want unsupported bundle provisioner name error")
	}
	if !strings.Contains(err.Error(), "unsupported bundle provisioner name") {
		t.Fatalf("ApplyInput() error = %v, want unsupported bundle provisioner name", err)
	}
}

func TestLoadFileAppliesConfigFileThenCommandLineInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chamberd.json")
	content := `{
		"http_addr": "127.0.0.1:9090",
		"tmp_root": "file/tmp",
		"privilege": "rootful",
		"bundle": { "root": "file/bundles" },
		"image": { "root": "file/images" },
		"runtime": {
			"runtime_root": "file/runtime",
			"name": "runc"
		},
		"open_telemetry_metrics_export_interval": 30000000000,
		"logging": { "level": "debug" }
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadFile(path, Input{
		HTTPAddr: ptr("127.0.0.1:7070"),
	}, mapGetenv(map[string]string{
		"HOME": t.TempDir(),
	}))
	if err != nil {
		t.Fatalf("LoadFile returned error: %v", err)
	}

	if cfg.HTTPAddr != "127.0.0.1:7070" {
		t.Fatalf("HTTPAddr = %q, want command-line input", cfg.HTTPAddr)
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
	if cfg.Runtime.Name != chamberRuntimeShared.RuntimeNameRunc {
		t.Fatalf("Runtime.Name = %q, want runc", cfg.Runtime.Name)
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

func TestMergeInputAppliesPrivilegeOverlays(t *testing.T) {
	base := Input{
		Privilege: ptr(capability.Rootless),
		Bundle: bundleInput{
			Privilege: ptr(capability.Rootless),
		},
		Runtime: runtimeInput{
			Privilege: ptr(capability.Rootless),
		},
	}
	overlay := Input{
		Privilege: ptr(capability.Rootful),
		Bundle: bundleInput{
			Privilege: ptr(capability.Rootful),
		},
		Runtime: runtimeInput{
			Privilege: ptr(capability.Rootful),
		},
	}

	merged := MergeInput(base, overlay)

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
