package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := DefaultConfig(root)

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
	_, err := newForGOOS(context.Background(), Config{
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
	tests := map[string]Config{
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
			_, err := newForGOOS(context.Background(), config, localfs.NewDirectoryManager(), "linux")
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

func TestSupportedNamesListsKnownImplementations(t *testing.T) {
	if !slices.Equal(SupportedNames(), []string{chamberRuntimeShared.RuntimeNameRunc}) {
		t.Fatalf("SupportedNames() = %#v, want runc", SupportedNames())
	}
	if !IsSupportedName(chamberRuntimeShared.RuntimeNameRunc) {
		t.Fatalf("IsSupportedName(%q) = false, want true", chamberRuntimeShared.RuntimeNameRunc)
	}
}

func TestSupportedCapabilitiesListsRuncCapabilities(t *testing.T) {
	capabilities, ok := SupportedCapabilities(chamberRuntimeShared.RuntimeNameRunc)
	if !ok {
		t.Fatalf("SupportedCapabilities(%q) ok = false, want true", chamberRuntimeShared.RuntimeNameRunc)
	}
	if !slices.Equal(capabilities.Privileges, []capability.Privilege{capability.Rootless}) {
		t.Fatalf("privileges = %#v, want rootless", capabilities.Privileges)
	}
	if !slices.Equal(capabilities.Isolation, []Isolation{chamberRuntimeShared.ProcessIsolation}) {
		t.Fatalf("isolation = %#v, want process", capabilities.Isolation)
	}
}

func TestNewRejectsUnsupportedPrivilegeBeforeFilesystemMutation(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	binDir := filepath.Join(t.TempDir(), "bin")

	_, err := New(context.Background(), Config{
		RuntimeRoot:   runtimeRoot,
		RuntimeBinDir: binDir,
		Name:          chamberRuntimeShared.RuntimeNameRunc,
		Privilege:     capability.Rootful,
	}, localfs.NewDirectoryManager())

	if err == nil {
		t.Fatal("New() error = nil, want unsupported privilege error")
	}
	if !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("New() error = %v, want unsupported privilege", err)
	}
	if _, statErr := os.Stat(runtimeRoot); !os.IsNotExist(statErr) {
		t.Fatalf("runtime root stat error = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(binDir); !os.IsNotExist(statErr) {
		t.Fatalf("runtime bin dir stat error = %v, want not exist", statErr)
	}
}

func TestNewRejectsNonLinuxHostBeforeFilesystemMutation(t *testing.T) {
	if goruntime.GOOS == "linux" {
		t.Skip("Linux hosts pass the shared OS gate")
	}
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	binDir := filepath.Join(t.TempDir(), "bin")

	_, err := New(context.Background(), Config{
		RuntimeRoot:   runtimeRoot,
		RuntimeBinDir: binDir,
		Name:          chamberRuntimeShared.RuntimeNameRunc,
		Privilege:     capability.Rootless,
	}, localfs.NewDirectoryManager())

	if err == nil {
		t.Fatal("New() error = nil, want Linux host requirement")
	}
	if !strings.Contains(err.Error(), "requires a Linux host or Linux VM") {
		t.Fatalf("New() error = %v, want Linux host explanation", err)
	}
	if _, statErr := os.Stat(runtimeRoot); !os.IsNotExist(statErr) {
		t.Fatalf("runtime root stat error = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(binDir); !os.IsNotExist(statErr) {
		t.Fatalf("runtime bin dir stat error = %v, want not exist", statErr)
	}
}
