// Package buildkit contains BuildKit-backed Dockerfile build mechanics for
// Chamber's image store.
package buildkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

type resolvedRequest struct {
	reference         string
	contextPath       string
	dockerfilePath    string
	dockerfileRelPath string
	platform          chamberImage.Platform
	target            string
	buildArgs         map[string]string
}

// ValidateRequest checks per-build inputs without preparing or running
// BuildKit.
func ValidateRequest(request chamberImage.BuildRequest) error {
	_, err := resolveRequest(request)
	return err
}

func resolveRequest(request chamberImage.BuildRequest) (resolvedRequest, error) {
	reference, err := chamberImage.CanonicalImageReference(request.Reference)
	if err != nil {
		return resolvedRequest{}, err
	}

	contextPath := strings.TrimSpace(request.ContextPath)
	if contextPath == "" {
		return resolvedRequest{}, fmt.Errorf("%w: build context path is required", chamberErrors.ErrInvalidRequest)
	}
	if !filepath.IsAbs(contextPath) {
		return resolvedRequest{}, fmt.Errorf("%w: build context path must be absolute", chamberErrors.ErrInvalidRequest)
	}
	contextInfo, err := os.Stat(contextPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resolvedRequest{}, fmt.Errorf("%w: build context path does not exist", chamberErrors.ErrInvalidRequest)
		}
		return resolvedRequest{}, fmt.Errorf("%w: inspect build context path: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if !contextInfo.IsDir() {
		return resolvedRequest{}, fmt.Errorf("%w: build context path must be a directory", chamberErrors.ErrInvalidRequest)
	}
	contextPath, err = filepath.EvalSymlinks(contextPath)
	if err != nil {
		return resolvedRequest{}, fmt.Errorf("%w: resolve build context path: %w", chamberErrors.ErrFilesystemFailed, err)
	}

	dockerfilePath := strings.TrimSpace(request.DockerfilePath)
	if dockerfilePath == "" {
		dockerfilePath = filepath.Join(contextPath, "Dockerfile")
	}
	if !filepath.IsAbs(dockerfilePath) {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must be absolute", chamberErrors.ErrInvalidRequest)
	}
	if !pathContains(contextPath, dockerfilePath) {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must be inside the build context", chamberErrors.ErrInvalidRequest)
	}
	dockerfileInfo, err := os.Stat(dockerfilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path does not exist", chamberErrors.ErrInvalidRequest)
		}
		return resolvedRequest{}, fmt.Errorf("%w: inspect Dockerfile path: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if dockerfileInfo.IsDir() {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must be a file", chamberErrors.ErrInvalidRequest)
	}
	dockerfilePath, err = filepath.EvalSymlinks(dockerfilePath)
	if err != nil {
		return resolvedRequest{}, fmt.Errorf("%w: resolve Dockerfile path: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if !pathContains(contextPath, dockerfilePath) {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must resolve inside the build context", chamberErrors.ErrInvalidRequest)
	}
	dockerfileRelPath, err := filepath.Rel(contextPath, dockerfilePath)
	if err != nil {
		return resolvedRequest{}, fmt.Errorf("%w: resolve Dockerfile relative path: %w", chamberErrors.ErrFilesystemFailed, err)
	}

	buildArgs := make(map[string]string, len(request.BuildArgs))
	for key, value := range request.BuildArgs {
		key = strings.TrimSpace(key)
		if key == "" {
			return resolvedRequest{}, fmt.Errorf("%w: build argument name is required", chamberErrors.ErrInvalidRequest)
		}
		buildArgs[key] = value
	}
	return resolvedRequest{
		reference:         reference,
		contextPath:       contextPath,
		dockerfilePath:    dockerfilePath,
		dockerfileRelPath: dockerfileRelPath,
		platform:          chamberImage.NormalizePlatform(request.Platform),
		target:            strings.TrimSpace(request.Target),
		buildArgs:         buildArgs,
	}, nil
}

func pathContains(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
