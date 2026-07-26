package buildkit

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

const legacyTarRegularFile = 0

func (b Builder) extractOCITar(tarPath string, destination string) error {
	file, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("%w: open BuildKit OCI archive: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	defer file.Close()

	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read BuildKit OCI archive: %w", chamberErrors.ErrBuildFailed, err)
		}
		target, err := safeTarPath(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := b.directoryManager.MkdirPrivate(target); err != nil {
				return fmt.Errorf("%w: create OCI archive directory: %w", chamberErrors.ErrFilesystemFailed, err)
			}
		case tar.TypeReg, legacyTarRegularFile:
			if err := b.directoryManager.MkdirPrivate(filepath.Dir(target)); err != nil {
				return fmt.Errorf("%w: create OCI archive file parent: %w", chamberErrors.ErrFilesystemFailed, err)
			}
			if err := writeFileAtomic(target, reader, 0600, b.directoryManager); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported OCI archive entry %q", chamberErrors.ErrBuildFailed, header.Name)
		}
	}
	return nil
}

func safeTarPath(root string, name string) (string, error) {
	clean := filepath.Clean(name)
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("%w: unsafe OCI archive path %q", chamberErrors.ErrBuildFailed, name)
	}
	target := filepath.Join(root, clean)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: unsafe OCI archive path %q", chamberErrors.ErrBuildFailed, name)
	}
	return target, nil
}

func writeExecutableAtomic(path string, reader io.Reader, directoryManager localfs.DirectoryManager) error {
	return writeFileAtomic(path, reader, 0755, directoryManager)
}

func writeFileAtomic(path string, reader io.Reader, mode os.FileMode, directoryManager localfs.DirectoryManager) error {
	if err := directoryManager.MkdirPrivate(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: create file parent: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmp, err := directoryManager.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, reader); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: write temporary file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: sync temporary file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: set temporary file mode: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close temporary file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("%w: commit temporary file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	committed = true
	return nil
}
