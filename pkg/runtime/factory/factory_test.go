package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func init() {
	checkRuntimeHostRules = func(context.Context) error { return nil }
}

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := chamberRuntime.DefaultConfig(root)

	if cfg.RuntimeRoot != filepath.Join(root, "run", "runtime") {
		t.Fatalf("RuntimeRoot = %q, want default runtime root", cfg.RuntimeRoot)
	}
	if cfg.RuntimeTmpRoot != hostfs.DefaultTmpRoot("runtime") {
		t.Fatalf("RuntimeTmpRoot = %q, want default runtime temp root", cfg.RuntimeTmpRoot)
	}
	if cfg.RuntimeBinTmpRoot != hostfs.DefaultTmpRoot("runtime-bin") {
		t.Fatalf("RuntimeBinTmpRoot = %q, want default runtime binary temp root", cfg.RuntimeBinTmpRoot)
	}
	if cfg.Name != chamberRuntime.RuntimeNameRunc {
		t.Fatalf("Name = %q, want runc", cfg.Name)
	}
}

func TestNewRuntimeCreatesConfiguredWorkspace(t *testing.T) {
	root := t.TempDir()
	runtimePath := filepath.Join(root, "runc")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatalf("WriteFile(runtimePath) error = %v", err)
	}
	config := chamberRuntime.Config{
		RuntimeRoot: filepath.Join(root, "runtime"),
		RuntimePath: runtimePath,
		Name:        chamberRuntime.RuntimeNameRunc,
	}

	runtime, err := NewRuntime(context.Background(), config)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	if runtime == nil {
		t.Fatal("NewRuntime() runtime = nil, want runtime")
	}
}

func TestNewRejectsUnsupportedRuntimeName(t *testing.T) {
	config := chamberRuntime.Config{
		RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
		RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
		Name:          "crun",
	}
	_, err := newRuntime(context.Background(), config, newTestWorkspace(t, config.RuntimeRoot), newTestWorkspace(t, config.RuntimeBinDir))
	if err == nil {
		t.Fatal("New() error = nil, want unsupported runtime name error")
	}
	if !strings.Contains(err.Error(), "unsupported runtime name") {
		t.Fatalf("New() error = %v, want unsupported runtime name", err)
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want invalid request code", err)
	}
}

func TestNewRequiresFinalRuntimeConfig(t *testing.T) {
	tests := map[string]chamberRuntime.Config{
		"name": {
			RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
			RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
		},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := newRuntime(context.Background(), config, newTestWorkspace(t, config.RuntimeRoot), newTestWorkspace(t, config.RuntimeBinDir))
			if err == nil {
				t.Fatal("New() error = nil, want final config validation error")
			}
			if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
				t.Fatalf("New() error = %v, want invalid request code", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("New() error = %v, want missing %s explanation", err, name)
			}
		})
	}
}

func TestNewRejectsHostProbeFailureWithErrorCode(t *testing.T) {
	config := chamberRuntime.Config{
		RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
		RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
		Name:          chamberRuntime.RuntimeNameRunc,
	}
	restore := replaceRuntimeHostRules(func(context.Context) error {
		return fmt.Errorf("%w: host probe failed", chamberErrors.ErrUnsupportedHost)
	})
	defer restore()

	_, err := newRuntime(context.Background(), config, newTestWorkspace(t, config.RuntimeRoot), newTestWorkspace(t, config.RuntimeBinDir))
	if err == nil {
		t.Fatal("New() error = nil, want host probe error")
	}
	if !errors.Is(err, chamberErrors.ErrUnsupportedHost) {
		t.Fatalf("New() error = %v, want unsupported host code", err)
	}
}

func TestNewRejectsMismatchedRuntimeWorkspaceRoot(t *testing.T) {
	config := chamberRuntime.Config{
		RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
		RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
		Name:          chamberRuntime.RuntimeNameRunc,
	}
	_, err := newRuntime(context.Background(), config, newTestWorkspace(t, filepath.Join(t.TempDir(), "other-runtime")), newTestWorkspace(t, config.RuntimeBinDir))
	if err == nil {
		t.Fatal("New() error = nil, want mismatch error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want invalid request code", err)
	}
}

func TestNewRejectsMismatchedRuntimeWorkspaceTmpRoot(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	binDir := filepath.Join(t.TempDir(), "bin")
	config := chamberRuntime.Config{
		RuntimeRoot:    runtimeRoot,
		RuntimeTmpRoot: filepath.Join(t.TempDir(), "other-runtime-tmp"),
		RuntimeBinDir:  binDir,
		Name:           chamberRuntime.RuntimeNameRunc,
	}
	_, err := newRuntime(context.Background(), config, newTestWorkspace(t, runtimeRoot), newTestWorkspace(t, binDir))
	if err == nil {
		t.Fatal("New() error = nil, want mismatch error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want invalid request code", err)
	}
}

func TestNewRejectsMismatchedRuntimeBinaryWorkspaceTmpRoot(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	binDir := filepath.Join(t.TempDir(), "bin")
	config := chamberRuntime.Config{
		RuntimeRoot:       runtimeRoot,
		RuntimeBinDir:     binDir,
		RuntimeBinTmpRoot: filepath.Join(t.TempDir(), "other-bin-tmp"),
		Name:              chamberRuntime.RuntimeNameRunc,
	}
	_, err := newRuntime(context.Background(), config, newTestWorkspace(t, runtimeRoot), newTestWorkspace(t, binDir))
	if err == nil {
		t.Fatal("New() error = nil, want mismatch error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want invalid request code", err)
	}
}

func newTestWorkspace(t *testing.T, root string) *hostfs.Workspace {
	t.Helper()
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    root,
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
		Requirements: hostfs.FeatureSet{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	return workspace
}

func replaceRuntimeHostRules(check func(context.Context) error) func() {
	previous := checkRuntimeHostRules
	checkRuntimeHostRules = check
	return func() {
		checkRuntimeHostRules = previous
	}
}
