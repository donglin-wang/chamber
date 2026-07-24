package factory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

type provisionerImplementation struct {
	name       string
	privilege  capability.Privilege
	privileges []capability.Privilege
}

var provisionerImplementations = []provisionerImplementation{
	{
		name:      chamberBundle.ProvisionerNameDirectory,
		privilege: capability.Rootless,
		privileges: []capability.Privilege{
			capability.Rootless,
		},
	},
}

func TestProvisionerImplementationsSatisfySharedConstructorContract(t *testing.T) {
	for _, implementation := range provisionerImplementations {
		t.Run(implementation.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "bundles")

			provisioner, err := NewProvisioner(chamberBundle.Config{
				Root:      root,
				Name:      implementation.name,
				Privilege: implementation.privilege,
			}, localfs.NewDirectoryManager())
			if err != nil {
				t.Fatalf("NewProvisioner() error = %v", err)
			}
			if provisioner == nil {
				t.Fatal("NewProvisioner() provisioner = nil, want provisioner")
			}
			assertPrivateDir(t, root)

			descriptor := provisioner.Descriptor()
			if descriptor.Name != implementation.name {
				t.Fatalf("Descriptor().Name = %q, want %q", descriptor.Name, implementation.name)
			}
			if !slices.Equal(descriptor.Capabilities.Privileges, implementation.privileges) {
				t.Fatalf("Descriptor().Capabilities.Privileges = %#v, want %#v", descriptor.Capabilities.Privileges, implementation.privileges)
			}
		})
	}
}

func TestProvisionerImplementationsRejectUnsupportedPrivilegeBeforeFilesystemMutation(t *testing.T) {
	for _, implementation := range provisionerImplementations {
		t.Run(implementation.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "bundles")

			_, err := NewProvisioner(chamberBundle.Config{
				Root:      root,
				Name:      implementation.name,
				Privilege: capability.Rootful,
			}, localfs.NewDirectoryManager())

			if err == nil {
				t.Fatal("NewProvisioner() error = nil, want unsupported privilege error")
			}
			if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
				t.Fatalf("NewProvisioner() error = %v, want invalid request code", err)
			}
			if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
				t.Fatalf("bundle root stat error = %v, want not exist", statErr)
			}
		})
	}
}

func TestSupportedProvisionerNamesListsKnownImplementations(t *testing.T) {
	want := make([]string, 0, len(provisionerImplementations))
	for _, implementation := range provisionerImplementations {
		want = append(want, implementation.name)
		if !IsSupportedProvisionerName(implementation.name) {
			t.Fatalf("IsSupportedProvisionerName(%q) = false, want true", implementation.name)
		}
	}
	sort.Strings(want)
	if !slices.Equal(SupportedProvisionerNames(), want) {
		t.Fatalf("SupportedProvisionerNames() = %#v, want %#v", SupportedProvisionerNames(), want)
	}
}

func TestSupportedProvisionerCapabilitiesListsKnownImplementations(t *testing.T) {
	for _, implementation := range provisionerImplementations {
		t.Run(implementation.name, func(t *testing.T) {
			capabilities, ok := SupportedProvisionerCapabilities(implementation.name)
			if !ok {
				t.Fatalf("SupportedProvisionerCapabilities(%q) ok = false, want true", implementation.name)
			}
			if !slices.Equal(capabilities.Privileges, implementation.privileges) {
				t.Fatalf("privileges = %#v, want %#v", capabilities.Privileges, implementation.privileges)
			}
		})
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
