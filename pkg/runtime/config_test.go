package runtime

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

func TestResolveAppliesLoggingOverride(t *testing.T) {
	root := t.TempDir()

	cfg, err := Resolve(DefaultConfig(root), Override{
		Logging: chamberLogging.Override{
			Level:  ptr("debug"),
			Format: ptr("text"),
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if cfg.RuntimeRoot != filepath.Join(root, "run", "runtime") {
		t.Fatalf("RuntimeRoot = %q, want default runtime root", cfg.RuntimeRoot)
	}
	if cfg.Name != RuntimeNameRunc {
		t.Fatalf("Name = %q, want runc", cfg.Name)
	}
	if cfg.Privilege != capability.Rootless {
		t.Fatalf("Privilege = %q, want rootless", cfg.Privilege)
	}
	if cfg.Logging != (chamberLogging.Config{Level: "debug", Format: "text"}) {
		t.Fatalf("Logging = %#v, want debug text", cfg.Logging)
	}
}

func TestResolveAppliesPrivilegeOverride(t *testing.T) {
	rootful := capability.Rootful

	cfg, err := Resolve(DefaultConfig(t.TempDir()), Override{
		Privilege: &rootful,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if cfg.Privilege != capability.Rootful {
		t.Fatalf("Privilege = %q, want rootful", cfg.Privilege)
	}
}

func TestResolveRejectsUnsupportedRuntimeName(t *testing.T) {
	_, err := Resolve(DefaultConfig(t.TempDir()), Override{
		Name: ptr("crun"),
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want unsupported runtime name error")
	}
	if !strings.Contains(err.Error(), "unsupported runtime name") {
		t.Fatalf("Resolve() error = %v, want unsupported runtime name", err)
	}
}

func TestSupportedNamesListsKnownImplementations(t *testing.T) {
	if !slices.Equal(SupportedNames(), []string{RuntimeNameRunc}) {
		t.Fatalf("SupportedNames() = %#v, want runc", SupportedNames())
	}
	if !IsSupportedName(RuntimeNameRunc) {
		t.Fatalf("IsSupportedName(%q) = false, want true", RuntimeNameRunc)
	}
}

func TestSupportedCapabilitiesListsRuncCapabilities(t *testing.T) {
	capabilities, ok := SupportedCapabilities(RuntimeNameRunc)
	if !ok {
		t.Fatalf("SupportedCapabilities(%q) ok = false, want true", RuntimeNameRunc)
	}
	if !slices.Equal(capabilities.Privileges, []capability.Privilege{capability.Rootless}) {
		t.Fatalf("privileges = %#v, want rootless", capabilities.Privileges)
	}
	if !slices.Equal(capabilities.Isolation, []Isolation{ProcessIsolation}) {
		t.Fatalf("isolation = %#v, want process", capabilities.Isolation)
	}
}

func TestNewRejectsUnsupportedPrivilegeBeforeFilesystemMutation(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	binDir := filepath.Join(t.TempDir(), "bin")

	_, err := New(context.Background(), Config{
		RuntimeRoot:   runtimeRoot,
		RuntimeBinDir: binDir,
		Name:          RuntimeNameRunc,
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
		Name:          RuntimeNameRunc,
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

func ptr(value string) *string {
	return &value
}
