package localfs

import (
	"fmt"
	"os"
	"path/filepath"
)

type DirectoryManager interface {
	EnsurePrivateDir(path string) error
	EnsurePrivateParent(path string) error
	MkdirTemp(parent string, pattern string) (string, error)
	CreateTemp(parent string, pattern string) (*os.File, error)
}

type OSDirectoryManager struct{}

func NewDirectoryManager() OSDirectoryManager {
	return OSDirectoryManager{}
}

func (OSDirectoryManager) EnsurePrivateDir(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		return ensurePrivateDirMetadata(path, info)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read private directory metadata %q: %w", path, err)
	}

	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create private directory %q: %w", path, err)
	}

	info, err = os.Stat(path)
	if err != nil {
		return fmt.Errorf("read private directory metadata %q: %w", path, err)
	}
	return ensurePrivateDirMetadata(path, info)
}

func ensurePrivateDirMetadata(path string, info os.FileInfo) error {
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("path %q must not be readable, writable, or executable by group or other users", path)
	}

	return nil
}

func (manager OSDirectoryManager) EnsurePrivateParent(path string) error {
	return manager.EnsurePrivateDir(filepath.Dir(path))
}

func (manager OSDirectoryManager) MkdirTemp(parent string, pattern string) (string, error) {
	if err := manager.EnsurePrivateDir(parent); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, pattern)
}

func (manager OSDirectoryManager) CreateTemp(parent string, pattern string) (*os.File, error) {
	if err := manager.EnsurePrivateDir(parent); err != nil {
		return nil, err
	}
	return os.CreateTemp(parent, pattern)
}
