package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestNewProvisionerRequiresDirectoryManager(t *testing.T) {
	if _, err := NewProvisioner(chamberBundleShared.Config{Root: t.TempDir()}, nil); err == nil {
		t.Fatal("NewProvisioner() error = nil, want directory manager error")
	}
}

func TestNewProvisionerRequiresFinalConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	tests := map[string]chamberBundleShared.Config{
		"name": {
			Root:      root,
			Privilege: capability.Rootless,
		},
		"privilege": {
			Root: root,
			Name: chamberBundleShared.ProvisionerNameDirectory,
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

	_, err := NewProvisioner(chamberBundleShared.Config{
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
