package localfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

type DirectoryManager interface {
	MkdirPrivate(path string) error
	MkdirPrivateParent(path string) error
	MkdirTemp(parent string, pattern string) (string, error)
	CreateTemp(parent string, pattern string) (*os.File, error)
}

type OSDirectoryManager struct{}

func NewDirectoryManager() OSDirectoryManager {
	return OSDirectoryManager{}
}

func (OSDirectoryManager) MkdirPrivate(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: private directory path is required", chamberErrors.ErrInvalidRequest)
	}
	info, err := os.Stat(path)
	if err == nil {
		return privateDirMetadata(path, info)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("%w: read private directory metadata %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}

	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("%w: create private directory %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}

	info, err = os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: read private directory metadata %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
	}
	return privateDirMetadata(path, info)
}

func privateDirMetadata(path string, info os.FileInfo) error {
	if !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", chamberErrors.ErrInvalidRequest, path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%w: path %q must not be readable, writable, or executable by group or other users", chamberErrors.ErrInvalidRequest, path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot determine owner for private directory %q", chamberErrors.ErrFilesystemFailed, path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: private directory %q must be owned by the current user", chamberErrors.ErrInvalidRequest, path)
	}

	return nil
}

func (manager OSDirectoryManager) MkdirPrivateParent(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: private child path is required", chamberErrors.ErrInvalidRequest)
	}
	return manager.MkdirPrivate(filepath.Dir(path))
}

func (manager OSDirectoryManager) MkdirTemp(parent string, pattern string) (string, error) {
	if err := manager.MkdirPrivate(parent); err != nil {
		return "", err
	}
	path, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", fmt.Errorf("%w: create temporary directory below %q: %w", chamberErrors.ErrFilesystemFailed, parent, err)
	}
	return path, nil
}

func (manager OSDirectoryManager) CreateTemp(parent string, pattern string) (*os.File, error) {
	if err := manager.MkdirPrivate(parent); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(parent, pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: create temporary file below %q: %w", chamberErrors.ErrFilesystemFailed, parent, err)
	}
	return file, nil
}
