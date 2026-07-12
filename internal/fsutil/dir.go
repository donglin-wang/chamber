package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create private directory %q: %w", path, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("read private directory metadata %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("path %q must not be readable, writable, or executable by group or other users", path)
	}

	return nil
}

func EnsurePrivateParent(path string) error {
	return EnsurePrivateDir(filepath.Dir(path))
}
