package factory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func init() {
	checkProvisionerHostRules = func(context.Context) error { return nil }
}

func TestNewProvisionerRequiresWorkspace(t *testing.T) {
	if _, err := NewProvisionerWithWorkspace(chamberBundle.Config{Root: t.TempDir()}, nil); err == nil {
		t.Fatal("NewProvisioner() error = nil, want workspace error")
	}
}

func TestNewProvisionerRejectsHostProbeFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	restore := replaceProvisionerHostRules(func(context.Context) error {
		return fmt.Errorf("%w: host probe failed", chamberErrors.ErrUnsupportedHost)
	})
	defer restore()

	_, err := NewProvisioner(chamberBundle.Config{
		Root: root,
		Name: chamberBundle.ProvisionerNameDirectory,
	})
	if err == nil {
		t.Fatal("NewProvisioner() error = nil, want host probe error")
	}
	if !errors.Is(err, chamberErrors.ErrUnsupportedHost) {
		t.Fatalf("NewProvisioner() error = %v, want unsupported host code", err)
	}
}

func TestNewProvisionerRequiresFinalConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	tests := map[string]chamberBundle.Config{
		"name": {
			Root: root,
		},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewProvisionerWithWorkspace(config, newTestWorkspace(t, root))
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
		Root: root,
		Name: "overlay",
	})

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
	_, err := NewProvisionerWithWorkspace(chamberBundle.Config{
		Root: filepath.Join(t.TempDir(), "bundles"),
		Name: chamberBundle.ProvisionerNameDirectory,
	}, newTestWorkspace(t, filepath.Join(t.TempDir(), "other-bundles")))
	if err == nil {
		t.Fatal("NewProvisioner() error = nil, want mismatch error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewProvisioner() error = %v, want invalid request code", err)
	}
}

func TestNewProvisionerRejectsMismatchedWorkspaceTmpRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	_, err := NewProvisionerWithWorkspace(chamberBundle.Config{
		Root:    root,
		TmpRoot: filepath.Join(t.TempDir(), "other-tmp"),
		Name:    chamberBundle.ProvisionerNameDirectory,
	}, newTestWorkspace(t, root))
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
		Requirements: hostfs.FeatureSet{
			PrivateDirs:           true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	return workspace
}

func replaceProvisionerHostRules(check func(context.Context) error) func() {
	previous := checkProvisionerHostRules
	checkProvisionerHostRules = check
	return func() {
		checkProvisionerHostRules = previous
	}
}
