package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metadataetcd "github.com/donglin-wang/chamber/internal/metadata/etcd"
	runcruntime "github.com/donglin-wang/chamber/internal/runtime/runc"
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
		case "Runtime":
			wantType = reflect.TypeOf(runcruntime.Override{})
		case "Metadata":
			wantType = reflect.TypeOf(metadataetcd.Override{})
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
		ImageRoot:     filepath.Join(root, "images"),
		ContainerRoot: filepath.Join(root, "containers"),
		Runtime: runcruntime.Config{
			RuntimeRoot:   filepath.Join(root, "run", "runtime"),
			RuntimeBinDir: filepath.Join(root, "bin"),
			Name:          "runc",
		},
		Metadata: metadataetcd.Config{
			DataDir: filepath.Join(root, "metadata", "etcd"),
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
	if cfg.ImageRoot != filepath.Join(root, "images") {
		t.Fatalf("ImageRoot = %q, want %q", cfg.ImageRoot, filepath.Join(root, "images"))
	}
	if cfg.SocketPath != filepath.Join(root, "run", "chamber.sock") {
		t.Fatalf("SocketPath = %q, want %q", cfg.SocketPath, filepath.Join(root, "run", "chamber.sock"))
	}
	if cfg.Runtime.RuntimeRoot != filepath.Join(root, "run", "runtime") {
		t.Fatalf("Runtime.RuntimeRoot = %q, want %q", cfg.Runtime.RuntimeRoot, filepath.Join(root, "run", "runtime"))
	}
	if cfg.Metadata.DataDir != filepath.Join(root, "metadata", "etcd") {
		t.Fatalf("Metadata.DataDir = %q, want %q", cfg.Metadata.DataDir, filepath.Join(root, "metadata", "etcd"))
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
		ImageRoot:     "default/images",
		ContainerRoot: "default/containers",
		Runtime: runcruntime.Config{
			RuntimeRoot:   "default/runtime",
			RuntimeBinDir: "default/bin",
			Name:          "default-runtime",
			Version:       "v0.0.1",
			URL:           "https://example.test/default-runtime",
			SHA256:        "default-sha",
		},
		Metadata: metadataetcd.Config{
			DataDir:      "default/metadata",
			ClientSocket: "default/metadata/client.sock",
			PeerSocket:   "default/metadata/peer.sock",
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
		ImageRoot:     ptr("override/images"),
		ContainerRoot: ptr("override/containers"),
		Runtime: runcruntime.Override{
			RuntimeRoot:   ptr("override/runtime"),
			RuntimeBinDir: ptr("override/bin"),
			Name:          ptr("crun"),
			Version:       ptr("v1.2.3"),
			URL:           ptr("https://example.test/runtime"),
			SHA256:        ptr("override-sha"),
		},
		Metadata: metadataetcd.Override{
			DataDir:      ptr("override/metadata"),
			ClientSocket: ptr("override/metadata/client.sock"),
			PeerSocket:   ptr("override/metadata/peer.sock"),
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
		ImageRoot:     mustAbs(t, "override/images"),
		ContainerRoot: mustAbs(t, "override/containers"),
		Runtime: runcruntime.Config{
			RuntimeRoot:   mustAbs(t, "override/runtime"),
			RuntimeBinDir: mustAbs(t, "override/bin"),
			Name:          "crun",
			Version:       "v1.2.3",
			URL:           "https://example.test/runtime",
			SHA256:        "override-sha",
		},
		Metadata: metadataetcd.Config{
			DataDir:      mustAbs(t, "override/metadata"),
			ClientSocket: mustAbs(t, "override/metadata/client.sock"),
			PeerSocket:   mustAbs(t, "override/metadata/peer.sock"),
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
		ImageRoot:     filepath.Join(root, "default", "images"),
		ContainerRoot: filepath.Join(root, "default", "containers"),
		Runtime: runcruntime.Config{
			RuntimeRoot:   filepath.Join(root, "default", "runtime"),
			RuntimeBinDir: filepath.Join(root, "default", "bin"),
			Name:          "default-runtime",
			Version:       "v0.0.1",
			URL:           "https://example.test/default-runtime",
			SHA256:        "default-sha",
		},
		Metadata: metadataetcd.Config{
			DataDir: filepath.Join(root, "default", "metadata"),
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

func TestPrepareCreatesPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		SocketPath:    filepath.Join(root, "run", "chamber.sock"),
		TmpRoot:       filepath.Join(root, "run", "tmp"),
		ImageRoot:     filepath.Join(root, "images"),
		ContainerRoot: filepath.Join(root, "containers"),
		Runtime: runcruntime.Config{
			RuntimeRoot:   filepath.Join(root, "run", "runtime"),
			RuntimeBinDir: filepath.Join(root, "bin"),
		},
		Metadata: metadataetcd.Config{
			DataDir: filepath.Join(root, "metadata", "etcd"),
		},
	}

	if err := cfg.Prepare(); err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}

	for _, path := range []string{
		filepath.Dir(cfg.SocketPath),
		cfg.TmpRoot,
		cfg.ImageRoot,
		cfg.ContainerRoot,
		cfg.Runtime.RuntimeRoot,
		cfg.Runtime.RuntimeBinDir,
		cfg.Metadata.DataDir,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q) returned error: %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%q is not a directory", path)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("%q permissions = %o, want 0700", path, info.Mode().Perm())
		}
	}
}

func TestPrepareRejectsGroupOrOtherAccessibleDirectory(t *testing.T) {
	root := t.TempDir()
	runRoot := filepath.Join(root, "run")
	tmpRoot := filepath.Join(root, "run", "tmp")
	if err := os.MkdirAll(runRoot, 0700); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.Mkdir(tmpRoot, 0755); err != nil {
		t.Fatalf("Mkdir returned error: %v", err)
	}
	if err := os.Chmod(tmpRoot, 0755); err != nil {
		t.Fatalf("Chmod returned error: %v", err)
	}

	cfg := Config{
		SocketPath:    filepath.Join(root, "run", "chamber.sock"),
		TmpRoot:       tmpRoot,
		ImageRoot:     filepath.Join(root, "images"),
		ContainerRoot: filepath.Join(root, "containers"),
		Runtime: runcruntime.Config{
			RuntimeRoot:   filepath.Join(root, "run", "runtime"),
			RuntimeBinDir: filepath.Join(root, "bin"),
		},
		Metadata: metadataetcd.Config{
			DataDir: filepath.Join(root, "metadata", "etcd"),
		},
	}

	err := cfg.Prepare()
	if err == nil {
		t.Fatal("Prepare returned nil error")
	}
	if !strings.Contains(err.Error(), "prepare tmp root") {
		t.Fatalf("Prepare error = %q, want tmp root context", err)
	}
	if !strings.Contains(err.Error(), "must not be readable, writable, or executable by group or other users") {
		t.Fatalf("Prepare error = %q, want permission explanation", err)
	}
}

func TestPrepareRejectsPathThatIsAFile(t *testing.T) {
	root := t.TempDir()
	imageRoot := filepath.Join(root, "images")
	if err := os.WriteFile(imageRoot, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	cfg := Config{
		SocketPath:    filepath.Join(root, "run", "chamber.sock"),
		TmpRoot:       filepath.Join(root, "run", "tmp"),
		ImageRoot:     imageRoot,
		ContainerRoot: filepath.Join(root, "containers"),
		Runtime: runcruntime.Config{
			RuntimeRoot:   filepath.Join(root, "run", "runtime"),
			RuntimeBinDir: filepath.Join(root, "bin"),
		},
		Metadata: metadataetcd.Config{
			DataDir: filepath.Join(root, "metadata", "etcd"),
		},
	}

	err := cfg.Prepare()
	if err == nil {
		t.Fatal("Prepare returned nil error")
	}
	if !strings.Contains(err.Error(), "prepare image root") {
		t.Fatalf("Prepare error = %q, want image root context", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Prepare error = %q, want file rejection", err)
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
