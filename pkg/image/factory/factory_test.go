package factory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestNewStorePreparesConfiguredImageRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")

	store, err := NewStore(chamberImage.Config{Root: root}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if store == nil {
		t.Fatal("NewStore() store = nil, want store")
	}
	assertPrivateDir(t, root)
	assertPrivateDir(t, filepath.Join(root, "layout"))
	assertPrivateDir(t, filepath.Join(root, "metadata"))
	assertPrivateDir(t, filepath.Join(root, "metadata", "images"))
	assertPrivateDir(t, filepath.Join(root, "tmp"))
	if _, err := os.Stat(filepath.Join(root, "layout", "oci-layout")); err != nil {
		t.Fatalf("Stat(oci-layout) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "layout", "index.json")); err != nil {
		t.Fatalf("Stat(index.json) error = %v", err)
	}
}

func TestNewStoreRequiresConfiguredImageRoot(t *testing.T) {
	if _, err := NewStore(chamberImage.Config{}, localfs.NewDirectoryManager()); err == nil {
		t.Fatal("NewStore() error = nil, want root required error")
	}
}

func TestNewStoreRequiresDirectoryManager(t *testing.T) {
	if _, err := NewStore(chamberImage.Config{}, nil); err == nil {
		t.Fatal("NewStore() error = nil, want directory manager error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewStore() error = %v, want invalid request code", err)
	}
}

func TestNewStoreWrapsImageRootSetupFailuresWithFilesystemCode(t *testing.T) {
	_, err := NewStore(chamberImage.Config{Root: filepath.Join(t.TempDir(), "images")}, failingDirectoryManager{err: errors.New("disk full")})
	if err == nil {
		t.Fatal("NewStore() error = nil, want filesystem error")
	}
	if !errors.Is(err, chamberErrors.ErrFilesystemFailed) {
		t.Fatalf("NewStore() error = %v, want filesystem failed code", err)
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
