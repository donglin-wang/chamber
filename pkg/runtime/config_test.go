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
	if cfg.Name != RuntimeNameRunc {
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
			Name:          RuntimeNameRunc,
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

func TestNewDispatchesRegisteredRuntimeConstructor(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	binDir := filepath.Join(t.TempDir(), "bin")
	called := false
	restoreImplementation := registerConstructorForTest(t, RuntimeNameRunc, func(ctx context.Context, config Config, directoryManager localfs.DirectoryManager) (Runtime, error) {
		called = true
		if config.Name != RuntimeNameRunc {
			t.Fatalf("constructor config name = %q, want %q", config.Name, RuntimeNameRunc)
		}
		if config.Privilege != capability.Rootless {
			t.Fatalf("constructor privilege = %q, want rootless", config.Privilege)
		}
		assertPrivateDir(t, config.RuntimeRoot)
		assertPrivateDir(t, config.RuntimeBinDir)
		return fakeRuntime{}, nil
	})
	defer restoreImplementation()

	runtime, err := newForGOOS(context.Background(), Config{
		RuntimeRoot:   runtimeRoot,
		RuntimeBinDir: binDir,
		Name:          RuntimeNameRunc,
		Privilege:     capability.Rootless,
	}, localfs.NewDirectoryManager(), "linux")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if runtime == nil {
		t.Fatal("New() runtime = nil, want runtime")
	}
	if !called {
		t.Fatal("registered constructor was not called")
	}
}

func registerConstructorForTest(t *testing.T, name string, constructor func(context.Context, Config, localfs.DirectoryManager) (Runtime, error)) func() {
	t.Helper()

	old := implementations[name]
	implementation := old
	implementation.New = nil
	implementations[name] = implementation
	Register(name, constructor)
	return func() {
		implementations[name] = old
	}
}

func assertPrivateDir(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode = %o, want 0700", info.Mode().Perm())
	}
}

type fakeRuntime struct{}

func (fakeRuntime) Descriptor() Descriptor {
	return Descriptor{Name: RuntimeNameRunc}
}

func (fakeRuntime) Binary() Binary {
	return Binary{}
}

func (fakeRuntime) Run(context.Context, RunRequest) (Process, error) {
	return nil, nil
}

func (fakeRuntime) State(context.Context, string) (ContainerState, error) {
	return ContainerState{}, nil
}

func (fakeRuntime) Signal(context.Context, SignalRequest) error {
	return nil
}

func (fakeRuntime) Delete(context.Context, DeleteRequest) error {
	return nil
}

func (fakeRuntime) ReadLog(string, LogStream) ([]byte, error) {
	return nil, nil
}
