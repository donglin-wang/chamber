package factory

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
)

type provisionerImplementation struct {
	name string
}

var provisionerImplementations = []provisionerImplementation{
	{
		name: chamberBundle.ProvisionerNameDirectory,
	},
}

func TestProvisionerImplementationsSatisfySharedConstructorContract(t *testing.T) {
	for _, implementation := range provisionerImplementations {
		t.Run(implementation.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "bundles")

			provisioner, err := NewProvisioner(chamberBundle.Config{
				Root: root,
				Name: implementation.name,
			}, newTestWorkspace(t, root))
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
