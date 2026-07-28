package hostfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestNewWorkspaceCreatesRootAndTempRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	tmpRoot := filepath.Join(t.TempDir(), "tmp")

	workspace, err := NewWorkspace(Config{
		Root:    root,
		TmpRoot: tmpRoot,
		Capabilities: Capabilities{
			PrivateDirs: true,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}

	if workspace.Root() != root {
		t.Fatalf("Root() = %q, want %q", workspace.Root(), root)
	}
	if workspace.TmpRoot() != tmpRoot {
		t.Fatalf("TmpRoot() = %q, want %q", workspace.TmpRoot(), tmpRoot)
	}
	assertPrivateDir(t, root)
	assertPrivateDir(t, tmpRoot)
}

func TestNewWorkspaceDefaultsTmpRootBelowOSTempDir(t *testing.T) {
	workspace, err := NewWorkspace(Config{Root: filepath.Join(t.TempDir(), "root")})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	if !strings.HasPrefix(workspace.TmpRoot(), filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatalf("TmpRoot() = %q, want below %q", workspace.TmpRoot(), os.TempDir())
	}
}

func TestDefaultTmpRootAddsOptionalNameBelowUserTempRoot(t *testing.T) {
	root := DefaultTmpRoot("")
	if !strings.HasPrefix(root, filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatalf("DefaultTmpRoot(\"\") = %q, want below %q", root, os.TempDir())
	}
	if got, want := DefaultTmpRoot("images"), filepath.Join(root, "images"); got != want {
		t.Fatalf("DefaultTmpRoot(\"images\") = %q, want %q", got, want)
	}
}

func TestNewWorkspaceRejectsUnsafeExistingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	_, err := NewWorkspace(Config{Root: root, Capabilities: Capabilities{PrivateDirs: true}})
	if err == nil {
		t.Fatal("NewWorkspace() error = nil")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("NewWorkspace() error = %v, want invalid request", err)
	}
}

func TestNewWorkspaceRecordsUnsafeExistingRootWhenPrivateDirsUnrequested(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	workspace, err := NewWorkspace(Config{Root: root})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	if workspace.Capabilities().PrivateDirs {
		t.Fatal("PrivateDirs = true, want false")
	}
}

func TestWorkspaceRejectsEscapingPaths(t *testing.T) {
	workspace := newTestWorkspace(t)
	for _, rel := range []string{"", "../outside", "nested/../../outside", filepath.Join(string(filepath.Separator), "abs")} {
		if _, err := workspace.MkdirPrivate(rel); err == nil {
			t.Fatalf("MkdirPrivate(%q) error = nil", rel)
		} else if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
			t.Fatalf("MkdirPrivate(%q) error = %v, want invalid request", rel, err)
		}
	}
}

func TestWorkspaceRejectsSymlinkEscape(t *testing.T) {
	workspace := newTestWorkspace(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatalf("Mkdir(outside) error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace.Root(), "link")); err != nil {
		t.Skipf("Symlink() error = %v", err)
	}

	_, err := workspace.CreatePrivate("link/escaped.txt")
	if err == nil {
		t.Fatal("CreatePrivate(symlink escape) error = nil")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("CreatePrivate(symlink escape) error = %v, want invalid request", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "escaped.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("escaped file stat error = %v, want not exist", statErr)
	}
}

func TestMkdirPrivateCreatesBelowRoot(t *testing.T) {
	workspace := newTestWorkspace(t)

	path, err := workspace.MkdirPrivate("layout/blobs")
	if err != nil {
		t.Fatalf("MkdirPrivate() error = %v", err)
	}
	if path != filepath.Join(workspace.Root(), "layout", "blobs") {
		t.Fatalf("MkdirPrivate() path = %q", path)
	}
	assertPrivateDir(t, path)
}

func TestCreatePrivateCreatesBelowRoot(t *testing.T) {
	workspace := newTestWorkspace(t)

	file, err := workspace.CreatePrivate("logs/container/stdout.log")
	if err != nil {
		t.Fatalf("CreatePrivate() error = %v", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if path != filepath.Join(workspace.Root(), "logs", "container", "stdout.log") {
		t.Fatalf("CreatePrivate() path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestCreatePrivateFailsWhenFileExists(t *testing.T) {
	workspace := newTestWorkspace(t)
	file, err := workspace.CreatePrivate("metadata/image.json")
	if err != nil {
		t.Fatalf("CreatePrivate() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	_, err = workspace.CreatePrivate("metadata/image.json")
	if err == nil {
		t.Fatal("CreatePrivate(existing) error = nil")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("CreatePrivate(existing) error = %v, want invalid request", err)
	}
}

func TestTempMethodsCreateBelowTmpRoot(t *testing.T) {
	workspace := newTestWorkspace(t)

	dir, err := workspace.MkdirTemp("pulls", ".pull-*")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	if filepath.Dir(dir) != filepath.Join(workspace.TmpRoot(), "pulls") {
		t.Fatalf("MkdirTemp() = %q, want below temp pulls dir", dir)
	}

	file, err := workspace.CreateTemp("layout", ".index.tmp-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if filepath.Dir(path) != filepath.Join(workspace.TmpRoot(), "layout") {
		t.Fatalf("CreateTemp() = %q, want below temp layout dir", path)
	}
}

func TestNewWorkspaceRecordsDirectoryFsyncUnsupportedWhenUnrequested(t *testing.T) {
	workspace, err := newWorkspace(Config{
		Root:    filepath.Join(t.TempDir(), "root"),
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
	}, filesystemOps{
		syncDir: func(string) error { return errDirectorySyncUnsupported },
	})
	if err != nil {
		t.Fatalf("newWorkspace() error = %v", err)
	}
	if workspace.Capabilities().DirectoryFsync {
		t.Fatal("DirectoryFsync = true, want false")
	}
}

func TestNewWorkspaceFailsWhenDirectoryFsyncRequiredAndUnsupported(t *testing.T) {
	_, err := newWorkspace(Config{
		Root:    filepath.Join(t.TempDir(), "root"),
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
		Capabilities: Capabilities{
			DirectoryFsync: true,
		},
	}, filesystemOps{
		syncDir: func(string) error { return errDirectorySyncUnsupported },
	})
	if err == nil {
		t.Fatal("newWorkspace() error = nil")
	}
	if !errors.Is(err, chamberErrors.ErrFilesystemFailed) {
		t.Fatalf("newWorkspace() error = %v, want filesystem failed", err)
	}
}

func TestNewWorkspaceFailsWhenRequiredFileRenameProbeFails(t *testing.T) {
	_, err := newWorkspace(Config{
		Root:    filepath.Join(t.TempDir(), "root"),
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
		Capabilities: Capabilities{
			AtomicFileRename: true,
		},
	}, filesystemOps{
		rename: func(src string, dst string) error {
			if !isDirectoryProbe(src, dst) {
				return syscall.EXDEV
			}
			return os.Rename(src, dst)
		},
	})
	if err == nil {
		t.Fatal("newWorkspace() error = nil")
	}
	if !errors.Is(err, chamberErrors.ErrFilesystemFailed) {
		t.Fatalf("newWorkspace() error = %v, want filesystem failed", err)
	}
}

func TestNewWorkspaceDoesNotFailWhenUnrequestedFileRenameProbeFails(t *testing.T) {
	workspace, err := newWorkspace(Config{
		Root:    filepath.Join(t.TempDir(), "root"),
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
	}, filesystemOps{
		rename: func(src string, dst string) error {
			if !isDirectoryProbe(src, dst) {
				return syscall.EXDEV
			}
			return os.Rename(src, dst)
		},
	})
	if err != nil {
		t.Fatalf("newWorkspace() error = %v", err)
	}
	if workspace.Capabilities().AtomicFileRename {
		t.Fatal("AtomicFileRename = true, want false")
	}
}

func TestNewWorkspaceFailsWhenRequiredDirectoryRenameProbeFails(t *testing.T) {
	_, err := newWorkspace(Config{
		Root:    filepath.Join(t.TempDir(), "root"),
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
		Capabilities: Capabilities{
			AtomicDirectoryRename: true,
		},
	}, filesystemOps{
		rename: func(src string, dst string) error {
			if isDirectoryProbe(src, dst) {
				return syscall.EXDEV
			}
			return os.Rename(src, dst)
		},
	})
	if err == nil {
		t.Fatal("newWorkspace() error = nil")
	}
	if !errors.Is(err, chamberErrors.ErrFilesystemFailed) {
		t.Fatalf("newWorkspace() error = %v, want filesystem failed", err)
	}
}

func newTestWorkspace(t *testing.T) *Workspace {
	t.Helper()
	workspace, err := NewWorkspace(Config{
		Root:    filepath.Join(t.TempDir(), "root"),
		TmpRoot: filepath.Join(t.TempDir(), "tmp"),
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	return workspace
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
		t.Fatalf("mode = %v, want 0700", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("file info does not contain Stat_t")
	}
	if int(stat.Uid) != os.Geteuid() {
		t.Fatalf("uid = %d, want %d", stat.Uid, os.Geteuid())
	}
}

func isDirectoryProbe(src string, dst string) bool {
	return strings.Contains(filepath.Base(src), ".hostfs-dir-src-") ||
		strings.Contains(filepath.Base(dst), ".hostfs-dir-dst-")
}
