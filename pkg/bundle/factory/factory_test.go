package factory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestNewProvisionerRequiresDirectoryManager(t *testing.T) {
	if _, err := NewProvisioner(chamberBundle.Config{Root: t.TempDir()}, nil); err == nil {
		t.Fatal("NewProvisioner() error = nil, want directory manager error")
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
			_, err := NewProvisioner(config, localfs.NewDirectoryManager())
			if err == nil {
				t.Fatal("NewProvisioner() error = nil, want final config validation error")
			}
			if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
				t.Fatalf("NewProvisioner() error = %v, want invalid request code", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("NewProvisioner() error = %v, want missing %s explanation", err, name)
			}
			if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
				t.Fatalf("bundle root stat error = %v, want not exist", statErr)
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
	}, localfs.NewDirectoryManager())

	if err == nil {
		t.Fatal("NewProvisioner() error = nil, want unsupported name error")
	}
	if !strings.Contains(err.Error(), "unsupported bundle provisioner name") {
		t.Fatalf("NewProvisioner() error = %v, want unsupported name", err)
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewProvisioner() error = %v, want invalid request code", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("bundle root stat error = %v, want not exist", statErr)
	}
}

func TestNewProvisionerWrapsBundleRootSetupFailuresWithFilesystemCode(t *testing.T) {
	_, err := NewProvisioner(chamberBundle.Config{
		Root:      filepath.Join(t.TempDir(), "bundles"),
		Name:      chamberBundle.ProvisionerNameDirectory,
		Privilege: capability.Rootless,
	}, failingDirectoryManager{err: errors.New("disk full")})
	if err == nil {
		t.Fatal("NewProvisioner() error = nil, want filesystem error")
	}
	if !errors.Is(err, chamberErrors.ErrFilesystemFailed) {
		t.Fatalf("NewProvisioner() error = %v, want filesystem failed code", err)
	}
}

type failingDirectoryManager struct {
	err error
}

func (manager failingDirectoryManager) MkdirPrivate(string) error {
	return manager.err
}

func (manager failingDirectoryManager) MkdirPrivateParent(string) error {
	return manager.err
}

func (manager failingDirectoryManager) MkdirTemp(string, string) (string, error) {
	return "", manager.err
}

func (manager failingDirectoryManager) CreateTemp(string, string) (*os.File, error) {
	return nil, manager.err
}
