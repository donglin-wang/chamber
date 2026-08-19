package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func init() {
	checkImageStoreHostRules = func(context.Context) error { return nil }
}

func TestNewStorePreparesConfiguredImageRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")

	store, err := NewStore(chamberImage.Config{Root: root})
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
	if _, err := os.Stat(filepath.Join(root, "layout", "oci-layout")); err != nil {
		t.Fatalf("Stat(oci-layout) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "layout", "index.json")); err != nil {
		t.Fatalf("Stat(index.json) error = %v", err)
	}
}

func TestNewStoreRejectsHostProbeFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	restore := replaceImageStoreHostRules(func(context.Context) error {
		return fmt.Errorf("%w: host probe failed", chamberErrors.ErrUnsupportedHost)
	})
	defer restore()

	_, err := NewStore(chamberImage.Config{Root: root})
	if err == nil {
		t.Fatal("NewStore() error = nil, want host probe error")
	}
	if !errors.Is(err, chamberErrors.ErrUnsupportedHost) {
		t.Fatalf("NewStore() error = %v, want unsupported host code", err)
	}
}

func TestNewStoreRequiresConfiguredImageRoot(t *testing.T) {
	if _, err := NewStoreWithWorkspace(chamberImage.Config{}, newTestWorkspace(t, filepath.Join(t.TempDir(), "images"))); err == nil {
		t.Fatal("NewStore() error = nil, want root required error")
	}
}

func TestNewStoreRequiresWorkspace(t *testing.T) {
	if _, err := NewStoreWithWorkspace(chamberImage.Config{}, nil); err == nil {
		t.Fatal("NewStore() error = nil, want workspace error")
	} else if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewStore() error = %v, want invalid request code", err)
	}
}

func TestNewStoreRejectsMismatchedWorkspaceRoot(t *testing.T) {
	_, err := NewStoreWithWorkspace(chamberImage.Config{Root: filepath.Join(t.TempDir(), "images")}, newTestWorkspace(t, filepath.Join(t.TempDir(), "other-images")))
	if err == nil {
		t.Fatal("NewStore() error = nil, want mismatch error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewStore() error = %v, want invalid request code", err)
	}
}

func TestNewStoreRejectsMismatchedWorkspaceTmpRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	_, err := NewStoreWithWorkspace(chamberImage.Config{
		Root:    root,
		TmpRoot: filepath.Join(t.TempDir(), "other-tmp"),
	}, newTestWorkspace(t, root))
	if err == nil {
		t.Fatal("NewStore() error = nil, want mismatch error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewStore() error = %v, want invalid request code", err)
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

func newTestWorkspace(t *testing.T, root string) *hostfs.Workspace {
	t.Helper()
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    root,
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
		Requirements: hostfs.FeatureSet{
			PrivateDirs:           true,
			FileFsync:             true,
			AtomicFileRename:      true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	return workspace
}

func replaceImageStoreHostRules(check func(context.Context) error) func() {
	previous := checkImageStoreHostRules
	checkImageStoreHostRules = check
	return func() {
		checkImageStoreHostRules = previous
	}
}
