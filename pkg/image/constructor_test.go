package image

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	chamberImageShared "github.com/donglin-wang/chamber/pkg/image/shared"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestNewPreparesConfiguredImageRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")

	puller, err := NewPuller(chamberImageShared.Config{Root: root}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if puller == nil {
		t.Fatal("New() puller = nil, want puller")
	}
	assertPrivateDir(t, root)
}

func TestNewRequiresConfiguredImageRoot(t *testing.T) {
	if _, err := NewPuller(chamberImageShared.Config{}, localfs.NewDirectoryManager()); err == nil {
		t.Fatal("New() error = nil, want root required error")
	}
}

func TestNewRequiresDirectoryManager(t *testing.T) {
	if _, err := NewPuller(chamberImageShared.Config{}, nil); err == nil {
		t.Fatal("New() error = nil, want directory manager error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want invalid request code", err)
	}
}

func TestNewWrapsImageRootSetupFailuresWithFilesystemCode(t *testing.T) {
	_, err := NewPuller(chamberImageShared.Config{Root: filepath.Join(t.TempDir(), "images")}, failingDirectoryManager{err: errors.New("disk full")})
	if err == nil {
		t.Fatal("New() error = nil, want filesystem error")
	}
	if !errors.Is(err, chamberErrors.ErrFilesystemFailed) {
		t.Fatalf("New() error = %v, want filesystem failed code", err)
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
