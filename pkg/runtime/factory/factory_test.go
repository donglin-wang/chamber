package factory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

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
	if cfg.Privilege != capability.Rootless {
		t.Fatalf("Privilege = %q, want rootless", cfg.Privilege)
	}
}

func TestNewRejectsUnsupportedRuntimeName(t *testing.T) {
	config := chamberRuntime.Config{
		RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
		RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
		Name:          "crun",
		Privilege:     capability.Rootless,
	}
	_, err := newRuntimeForOS(context.Background(), config, newTestWorkspace(t, config.RuntimeRoot), newTestWorkspace(t, config.RuntimeBinDir), "linux")
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
			Privilege:     capability.Rootless,
		},
		"privilege": {
			RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
			RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
			Name:          chamberRuntime.RuntimeNameRunc,
		},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := newRuntimeForOS(context.Background(), config, newTestWorkspace(t, config.RuntimeRoot), newTestWorkspace(t, config.RuntimeBinDir), "linux")
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

func TestNewRejectsUnsupportedHostWithErrorCode(t *testing.T) {
	config := chamberRuntime.Config{
		RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
		RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
		Name:          chamberRuntime.RuntimeNameRunc,
		Privilege:     capability.Rootless,
	}
	_, err := newRuntimeForOS(context.Background(), config, newTestWorkspace(t, config.RuntimeRoot), newTestWorkspace(t, config.RuntimeBinDir), "darwin")
	if err == nil {
		t.Fatal("New() error = nil, want unsupported host error")
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
		Privilege:     capability.Rootless,
	}
	_, err := newRuntimeForOS(context.Background(), config, newTestWorkspace(t, filepath.Join(t.TempDir(), "other-runtime")), newTestWorkspace(t, config.RuntimeBinDir), "linux")
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
		Privilege:      capability.Rootless,
	}
	_, err := newRuntimeForOS(context.Background(), config, newTestWorkspace(t, runtimeRoot), newTestWorkspace(t, binDir), "linux")
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
		Privilege:         capability.Rootless,
	}
	_, err := newRuntimeForOS(context.Background(), config, newTestWorkspace(t, runtimeRoot), newTestWorkspace(t, binDir), "linux")
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
		Capabilities: hostfs.Capabilities{
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
