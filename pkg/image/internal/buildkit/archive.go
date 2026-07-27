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
			if err := b.mkdirPrivatePath(target); err != nil {
				return fmt.Errorf("%w: create OCI archive directory: %w", chamberErrors.ErrFilesystemFailed, err)
			}
		case tar.TypeReg, legacyTarRegularFile:
			if err := b.mkdirPrivatePath(filepath.Dir(target)); err != nil {
				return fmt.Errorf("%w: create OCI archive file parent: %w", chamberErrors.ErrFilesystemFailed, err)
			}
			if err := b.writeFileAtomic(target, reader, 0600); err != nil {
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

func (b Builder) writeExecutableAtomic(path string, reader io.Reader) error {
	return b.writeFileAtomic(path, reader, 0755)
}

func (b Builder) writeFileAtomic(path string, reader io.Reader, mode os.FileMode) error {
	if err := b.mkdirPrivatePath(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: create file parent: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tmp, err := b.createTempForPath(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
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

func (b Builder) mkdirPrivatePath(path string) error {
	if rel, ok := relBelow(b.workspace.Root(), path); ok {
		_, err := b.workspace.MkdirPrivate(rel)
		return err
	}
	if _, ok := relBelow(b.workspace.TmpRoot(), path); ok {
		return os.MkdirAll(path, 0700)
	}
	return fmt.Errorf("%w: path %q is outside image workspace", chamberErrors.ErrInvalidRequest, path)
}

func (b Builder) createTempForPath(parent string, pattern string) (*os.File, error) {
	if rel, ok := relBelow(b.workspace.Root(), parent); ok {
		return b.workspace.CreateTemp(rel, pattern)
	}
	if rel, ok := relBelow(b.workspace.TmpRoot(), parent); ok {
		return b.workspace.CreateTemp(rel, pattern)
	}
	return nil, fmt.Errorf("%w: path %q is outside image workspace", chamberErrors.ErrInvalidRequest, parent)
}

func relBelow(root string, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
