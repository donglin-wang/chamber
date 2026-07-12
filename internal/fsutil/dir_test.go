package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePrivateDirCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")

	if err := EnsurePrivateDir(path); err != nil {
		t.Fatalf("EnsurePrivateDir() error = %v", err)
	}

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

func TestEnsurePrivateDirRejectsGroupOrOtherAccessibleDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	err := EnsurePrivateDir(path)
	if err == nil {
		t.Fatal("EnsurePrivateDir() error = nil")
	}
	if !strings.Contains(err.Error(), "must not be readable, writable, or executable by group or other users") {
		t.Fatalf("EnsurePrivateDir() error = %v, want permission explanation", err)
	}
}

func TestEnsurePrivateDirRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := EnsurePrivateDir(path)
	if err == nil {
		t.Fatal("EnsurePrivateDir() error = nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("EnsurePrivateDir() error = %v, want file rejection", err)
	}
}

func TestEnsurePrivateParentCreatesParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent", "file")

	if err := EnsurePrivateParent(path); err != nil {
		t.Fatalf("EnsurePrivateParent() error = %v", err)
	}

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(parent) error = %v", err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("parent mode = %o, want 0700", info.Mode().Perm())
	}
}
