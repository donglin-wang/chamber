package buildah

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
		return fmt.Errorf("%w: open Buildah OCI archive: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	defer file.Close()

	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read Buildah OCI archive: %w", chamberErrors.ErrBuildFailed, err)
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
			if err := writeTarFile(target, reader); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported OCI archive entry %q", chamberErrors.ErrBuildFailed, header.Name)
		}
	}
	return nil
}

func writeTarFile(path string, reader io.Reader) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("%w: create OCI archive file: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	_, copyErr := io.Copy(file, reader)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("%w: write OCI archive file: %w", chamberErrors.ErrFilesystemFailed, copyErr)
	}
	if syncErr != nil {
		return fmt.Errorf("%w: sync OCI archive file: %w", chamberErrors.ErrFilesystemFailed, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("%w: close OCI archive file: %w", chamberErrors.ErrFilesystemFailed, closeErr)
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
