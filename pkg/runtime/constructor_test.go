package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := chamberRuntimeShared.DefaultConfig(root)

	if cfg.RuntimeRoot != filepath.Join(root, "run", "runtime") {
		t.Fatalf("RuntimeRoot = %q, want default runtime root", cfg.RuntimeRoot)
	}
	if cfg.Name != chamberRuntimeShared.RuntimeNameRunc {
		t.Fatalf("Name = %q, want runc", cfg.Name)
	}
	if cfg.Privilege != capability.Rootless {
		t.Fatalf("Privilege = %q, want rootless", cfg.Privilege)
	}
}

func TestNewRejectsUnsupportedRuntimeName(t *testing.T) {
	_, err := newRuntimeForOS(context.Background(), chamberRuntimeShared.Config{
		RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
		RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
		Name:          "crun",
		Privilege:     capability.Rootless,
	}, localfs.NewDirectoryManager(), "linux")
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
	tests := map[string]chamberRuntimeShared.Config{
		"name": {
			RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
			RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
			Privilege:     capability.Rootless,
		},
		"privilege": {
			RuntimeRoot:   filepath.Join(t.TempDir(), "runtime"),
			RuntimeBinDir: filepath.Join(t.TempDir(), "bin"),
			Name:          chamberRuntimeShared.RuntimeNameRunc,
		},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := newRuntimeForOS(context.Background(), config, localfs.NewDirectoryManager(), "linux")
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
