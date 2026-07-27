package factory

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func TestNewProvisionerRequiresWorkspace(t *testing.T) {
	if _, err := NewProvisioner(chamberBundle.Config{Root: t.TempDir()}, nil); err == nil {
		t.Fatal("NewProvisioner() error = nil, want workspace error")
	}
}

func TestNewProvisionerRequiresFinalConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	tests := map[string]chamberBundle.Config{
		"name": {
			Root:      root,
			Privilege: capability.Rootless,
		},
		"privilege": {
			Root: root,
			Name: chamberBundle.ProvisionerNameDirectory,
		},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewProvisioner(config, newTestWorkspace(t, root))
			if err == nil {
				t.Fatal("NewProvisioner() error = nil, want final config validation error")
			}
			if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
				t.Fatalf("NewProvisioner() error = %v, want invalid request code", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("NewProvisioner() error = %v, want missing %s explanation", err, name)
			}
		})
	}
}

func TestNewProvisionerRejectsUnsupportedName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")

	_, err := NewProvisioner(chamberBundle.Config{
		Root:      root,
		Name:      "overlay",
		Privilege: capability.Rootless,
	}, newTestWorkspace(t, root))

	if err == nil {
		t.Fatal("NewProvisioner() error = nil, want unsupported name error")
	}
	if !strings.Contains(err.Error(), "unsupported bundle provisioner name") {
		t.Fatalf("NewProvisioner() error = %v, want unsupported name", err)
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewProvisioner() error = %v, want invalid request code", err)
	}
}

func TestNewProvisionerRejectsMismatchedWorkspaceRoot(t *testing.T) {
	_, err := NewProvisioner(chamberBundle.Config{
		Root:      filepath.Join(t.TempDir(), "bundles"),
		Name:      chamberBundle.ProvisionerNameDirectory,
		Privilege: capability.Rootless,
	}, newTestWorkspace(t, filepath.Join(t.TempDir(), "other-bundles")))
	if err == nil {
		t.Fatal("NewProvisioner() error = nil, want mismatch error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewProvisioner() error = %v, want invalid request code", err)
	}
}

func newTestWorkspace(t *testing.T, root string) *hostfs.Workspace {
	t.Helper()
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    root,
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
		Capabilities: hostfs.Capabilities{
			PrivateDirs:           true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	return workspace
}
