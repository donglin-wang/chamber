package localfs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestEnsurePrivateDirCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	manager := NewDirectoryManager()

	if err := manager.EnsurePrivateDir(path); err != nil {
		t.Fatalf("EnsurePrivateDir() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode = %v, want 0700", info.Mode().Perm())
	}
	assertOwnedByCurrentUser(t, info)
}

func TestEnsurePrivateDirRejectsGroupOrOtherAccessibleDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	manager := NewDirectoryManager()
	err := manager.EnsurePrivateDir(path)
	if err == nil {
		t.Fatal("EnsurePrivateDir() error = nil")
	}
	if !strings.Contains(err.Error(), "must not be readable, writable, or executable by group or other users") {
		t.Fatalf("EnsurePrivateDir() error = %v, want permission explanation", err)
	}
}

func TestEnsurePrivateDirAcceptsExistingCurrentUserOwnedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	manager := NewDirectoryManager()
	if err := manager.EnsurePrivateDir(path); err != nil {
		t.Fatalf("EnsurePrivateDir() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	assertOwnedByCurrentUser(t, info)
}

func TestEnsurePrivateDirRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	manager := NewDirectoryManager()
	err := manager.EnsurePrivateDir(path)
	if err == nil {
		t.Fatal("EnsurePrivateDir() error = nil")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("EnsurePrivateDir() error = %v, want file rejection", err)
	}
}

func TestEnsurePrivateParentCreatesParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent", "file")
	manager := NewDirectoryManager()

	if err := manager.EnsurePrivateParent(path); err != nil {
		t.Fatalf("EnsurePrivateParent() error = %v", err)
	}

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(parent) error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", filepath.Dir(path))
	}
}

func assertOwnedByCurrentUser(t *testing.T, info os.FileInfo) {
	t.Helper()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("file info does not contain Stat_t")
	}
	if int(stat.Uid) != os.Geteuid() {
		t.Fatalf("uid = %d, want %d", stat.Uid, os.Geteuid())
	}
}

func TestMkdirTempCreatesDirectoryBelowPrivateParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "tmp")
	manager := NewDirectoryManager()

	path, err := manager.MkdirTemp(parent, "layout-*")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	if filepath.Dir(path) != parent {
		t.Fatalf("MkdirTemp() path = %q, want below %q", path, parent)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(temp dir) error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
}

func TestCreateTempCreatesFileBelowPrivateParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "tmp")
	manager := NewDirectoryManager()

	file, err := manager.CreateTemp(parent, "binary-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if filepath.Dir(path) != parent {
		t.Fatalf("CreateTemp() path = %q, want below %q", path, parent)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(temp file) error = %v", err)
	}
	if info.IsDir() {
		t.Fatalf("%q is a directory", path)
	}
}
