package buildkit

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

const (
	defaultBuildKitVersion = "v0.31.2"

	defaultBuildKitAMD64URL    = "https://github.com/moby/buildkit/releases/download/v0.31.2/buildkit-v0.31.2.linux-amd64.tar.gz"
	defaultBuildKitAMD64SHA256 = "fbabdb72433a35f5bb646e4cd424bf8567e5d055710cf55840f7af2020640791"
	defaultBuildKitARM64URL    = "https://github.com/moby/buildkit/releases/download/v0.31.2/buildkit-v0.31.2.linux-arm64.tar.gz"
	defaultBuildKitARM64SHA256 = "41fba1eed480376934fa4c8177ddd7021036b5168a0eb8e7ab5eccdf75d47a05"

	defaultRootlessKitVersion = "v3.0.2"

	defaultRootlessKitAMD64URL    = "https://github.com/rootless-containers/rootlesskit/releases/download/v3.0.2/rootlesskit-x86_64.tar.gz"
	defaultRootlessKitAMD64SHA256 = "f4f2764cdd99db4f3fa715acac9d760c49a5e7c2838f180bdbe3188cec248dfb"
	defaultRootlessKitARM64URL    = "https://github.com/rootless-containers/rootlesskit/releases/download/v3.0.2/rootlesskit-aarch64.tar.gz"
	defaultRootlessKitARM64SHA256 = "823ce5dc80fe24da6c1b1e3c37a70fa6e8ba3067636d9a763228031ffeb43dc8"

	defaultRuncVersion = "v1.5.0"

	defaultRuncAMD64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.amd64"
	defaultRuncAMD64SHA256 = "0363e69bebd3a027d1239364ab9b4f4873f6bc4e7a7878e94b4ea59f08551297"
	defaultRuncARM64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.arm64"
	defaultRuncARM64SHA256 = "1f6d8c553add066a6aaf838d3172d4c5ed3c6b065b6f7eed2f4a4aa4af261e59"
)

type managedArchive struct {
	version string
	url     string
	sha256  string
}

type managedBinary struct {
	version string
	url     string
	sha256  string
}

type toolPaths struct {
	buildctl    string
	buildkitd   string
	rootlesskit string
	runc        string
}

func (b Builder) installTools(ctx context.Context) (toolPaths, error) {
	if err := validateConfig(b.config); err != nil {
		return toolPaths{}, err
	}
	binDir := filepath.Join(b.root, "bin")
	if _, err := b.workspace.MkdirPrivate("bin"); err != nil {
		return toolPaths{}, fmt.Errorf("%w: create BuildKit tool directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}

	buildctlPath := filepath.Join(binDir, "buildctl")
	buildkitdPath := filepath.Join(binDir, "buildkitd")
	if b.config.BuildctlPath != "" || b.config.BuildkitdPath != "" {
		var err error
		buildctlPath, err = b.localTool(ctx, "buildctl", b.config.BuildctlPath)
		if err != nil {
			return toolPaths{}, err
		}
		buildkitdPath, err = b.localTool(ctx, "buildkitd", b.config.BuildkitdPath)
		if err != nil {
			return toolPaths{}, err
		}
	} else if err := b.managedBuildKit(ctx, binDir, buildctlPath, buildkitdPath); err != nil {
		return toolPaths{}, err
	}

	rootlesskitPath := filepath.Join(binDir, "rootlesskit")
	if strings.TrimSpace(b.config.RootlessKitPath) != "" {
		var err error
		rootlesskitPath, err = b.localTool(ctx, "rootlesskit", b.config.RootlessKitPath)
		if err != nil {
			return toolPaths{}, err
		}
	} else if err := b.managedRootlessKit(ctx, binDir, rootlesskitPath); err != nil {
		return toolPaths{}, err
	}

	runcPath := filepath.Join(binDir, "runc")
	if strings.TrimSpace(b.config.RuncPath) != "" {
		var err error
		runcPath, err = b.localTool(ctx, "runc", b.config.RuncPath)
		if err != nil {
			return toolPaths{}, err
		}
	} else if err := b.managedRunc(ctx, binDir, runcPath); err != nil {
		return toolPaths{}, err
	}

	return toolPaths{
		buildctl:    buildctlPath,
		buildkitd:   buildkitdPath,
		rootlesskit: rootlesskitPath,
		runc:        runcPath,
	}, nil
}

func validateConfig(config chamberImage.BuildKitConfig) error {
	snapshotter := strings.TrimSpace(config.Snapshotter)
	if snapshotter != "" && snapshotter != defaultSnapshotter {
		return fmt.Errorf("%w: unsupported BuildKit snapshotter %q", chamberErrors.ErrInvalidRequest, config.Snapshotter)
	}
	if (strings.TrimSpace(config.BuildctlPath) == "") != (strings.TrimSpace(config.BuildkitdPath) == "") {
		return fmt.Errorf("%w: BuildKit local tools require both buildctl_path and buildkitd_path", chamberErrors.ErrInvalidRequest)
	}
	for name, path := range map[string]string{
		"buildctl":    config.BuildctlPath,
		"buildkitd":   config.BuildkitdPath,
		"rootlesskit": config.RootlessKitPath,
		"runc":        config.RuncPath,
	} {
		if strings.TrimSpace(path) != "" && !filepath.IsAbs(strings.TrimSpace(path)) {
			return fmt.Errorf("%w: %s path must be absolute", chamberErrors.ErrInvalidRequest, name)
		}
	}
	if err := validateArchiveOverride("BuildKit", config.BuildKitVersion, config.BuildKitURL, config.BuildKitSHA256); err != nil {
		return err
	}
	if err := validateArchiveOverride("RootlessKit", config.RootlessKitVersion, config.RootlessKitURL, config.RootlessKitSHA256); err != nil {
		return err
	}
	if strings.TrimSpace(config.BuildctlPath) == "" {
		if _, err := buildKitArchive(config); err != nil {
			return err
		}
	}
	if strings.TrimSpace(config.RootlessKitPath) == "" {
		if _, err := rootlessKitArchive(config); err != nil {
			return err
		}
	}
	return nil
}

func validateArchiveOverride(name string, version string, url string, sha256 string) error {
	version = strings.TrimSpace(version)
	url = strings.TrimSpace(url)
	sha256 = strings.TrimSpace(sha256)
	if version != "" || url != "" || sha256 != "" {
		if version == "" || url == "" || sha256 == "" {
			return fmt.Errorf("%w: managed %s download requires version, url, and sha256", chamberErrors.ErrInvalidRequest, name)
		}
	}
	return nil
}

func (b Builder) localTool(ctx context.Context, name string, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%w: %s path is required", chamberErrors.ErrInvalidRequest, name)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %s path must be absolute", chamberErrors.ErrInvalidRequest, name)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s path does not exist", chamberErrors.ErrInvalidRequest, name)
		}
		return "", fmt.Errorf("%w: inspect %s path: %w", chamberErrors.ErrFilesystemFailed, name, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s path must be a file", chamberErrors.ErrInvalidRequest, name)
	}
	if err := probeTool(ctx, path); err != nil {
		return "", fmt.Errorf("%w: probe %s: %w", chamberErrors.ErrBuildInstallFailed, name, err)
	}
	return path, nil
}

func (b Builder) managedBuildKit(ctx context.Context, binDir string, buildctlPath string, buildkitdPath string) error {
	if err := preflightInstallHost(); err != nil {
		return err
	}
	if probeTool(ctx, buildctlPath) == nil && probeTool(ctx, buildkitdPath) == nil {
		return nil
	}
	archive, err := buildKitArchive(b.config)
	if err != nil {
		return err
	}
	return b.downloadAndExtractArchive(ctx, archive, binDir, map[string]string{
		"bin/buildctl":                buildctlPath,
		"bin/buildkit-cni-bridge":     "",
		"bin/buildkit-cni-firewall":   "",
		"bin/buildkit-cni-host-local": "",
		"bin/buildkit-cni-loopback":   "",
		"bin/buildkit-qemu-aarch64":   "",
		"bin/buildkit-qemu-arm":       "",
		"bin/buildkit-qemu-i386":      "",
		"bin/buildkit-qemu-ppc64le":   "",
		"bin/buildkit-qemu-riscv64":   "",
		"bin/buildkit-qemu-s390x":     "",
		"bin/buildkit-qemu-x86_64":    "",
		"bin/buildkit-runc":           "",
		"bin/buildkitd":               buildkitdPath,
	}, "BuildKit")
}

func (b Builder) managedRootlessKit(ctx context.Context, binDir string, rootlesskitPath string) error {
	if err := preflightInstallHost(); err != nil {
		return err
	}
	if probeTool(ctx, rootlesskitPath) == nil {
		return nil
	}
	archive, err := rootlessKitArchive(b.config)
	if err != nil {
		return err
	}
	return b.downloadAndExtractArchive(ctx, archive, binDir, map[string]string{
		"rootlessctl":              "",
		"rootlesskit":              rootlesskitPath,
		"rootlesskit-docker-proxy": "",
	}, "RootlessKit")
}

func (b Builder) managedRunc(ctx context.Context, binDir string, runcPath string) error {
	if err := preflightInstallHost(); err != nil {
		return err
	}
	binary, err := runcBinary()
	if err != nil {
		return err
	}
	expectedDigest, err := parseSHA256(binary.sha256, "runc")
	if err != nil {
		return err
	}
	if ok, err := fileMatchesSHA256(runcPath, expectedDigest); err != nil {
		return fmt.Errorf("%w: verify existing runc: %w", chamberErrors.ErrBuildInstallFailed, err)
	} else if ok {
		if err := os.Chmod(runcPath, 0755); err != nil {
			return fmt.Errorf("%w: make existing runc executable: %w", chamberErrors.ErrBuildInstallFailed, err)
		}
		return nil
	}
	return b.downloadBinary(ctx, binary.url, expectedDigest, binDir, runcPath, "runc")
}

func buildKitArchive(config chamberImage.BuildKitConfig) (managedArchive, error) {
	return configuredArchive("BuildKit", config.BuildKitVersion, config.BuildKitURL, config.BuildKitSHA256, func() (managedArchive, error) {
		switch runtimeArch() {
		case "amd64":
			return managedArchive{version: defaultBuildKitVersion, url: defaultBuildKitAMD64URL, sha256: defaultBuildKitAMD64SHA256}, nil
		case "arm64":
			return managedArchive{version: defaultBuildKitVersion, url: defaultBuildKitARM64URL, sha256: defaultBuildKitARM64SHA256}, nil
		default:
			return managedArchive{}, fmt.Errorf("%w: BuildKit does not have a default archive for architecture %q", chamberErrors.ErrUnsupportedHost, runtimeArch())
		}
	})
}

func rootlessKitArchive(config chamberImage.BuildKitConfig) (managedArchive, error) {
	return configuredArchive("RootlessKit", config.RootlessKitVersion, config.RootlessKitURL, config.RootlessKitSHA256, func() (managedArchive, error) {
		switch runtimeArch() {
		case "amd64":
			return managedArchive{version: defaultRootlessKitVersion, url: defaultRootlessKitAMD64URL, sha256: defaultRootlessKitAMD64SHA256}, nil
		case "arm64":
			return managedArchive{version: defaultRootlessKitVersion, url: defaultRootlessKitARM64URL, sha256: defaultRootlessKitARM64SHA256}, nil
		default:
			return managedArchive{}, fmt.Errorf("%w: RootlessKit does not have a default archive for architecture %q", chamberErrors.ErrUnsupportedHost, runtimeArch())
		}
	})
}

func configuredArchive(name string, version string, url string, sha256 string, defaults func() (managedArchive, error)) (managedArchive, error) {
	version = strings.TrimSpace(version)
	url = strings.TrimSpace(url)
	sha256 = strings.TrimSpace(sha256)
	if err := validateArchiveOverride(name, version, url, sha256); err != nil {
		return managedArchive{}, err
	}
	if version != "" {
		return managedArchive{version: version, url: url, sha256: sha256}, nil
	}
	return defaults()
}

func runcBinary() (managedBinary, error) {
	switch runtimeArch() {
	case "amd64":
		return managedBinary{version: defaultRuncVersion, url: defaultRuncAMD64URL, sha256: defaultRuncAMD64SHA256}, nil
	case "arm64":
		return managedBinary{version: defaultRuncVersion, url: defaultRuncARM64URL, sha256: defaultRuncARM64SHA256}, nil
	default:
		return managedBinary{}, fmt.Errorf("%w: runc does not have a default binary for architecture %q", chamberErrors.ErrUnsupportedHost, runtimeArch())
	}
}

func (b Builder) downloadAndExtractArchive(ctx context.Context, archive managedArchive, tempDir string, expected map[string]string, label string) error {
	expectedDigest, err := parseSHA256(archive.sha256, label)
	if err != nil {
		return err
	}
	archivePath, cleanup, err := b.downloadArchive(ctx, archive.url, expectedDigest, tempDir, label)
	if err != nil {
		return err
	}
	defer cleanup()
	return b.extractManagedArchive(archivePath, expected, label)
}

func (b Builder) downloadArchive(ctx context.Context, url string, expectedDigest []byte, tempDir string, label string) (string, func(), error) {
	client := b.client
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("%w: create %s download request: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", nil, fmt.Errorf("%w: %s download canceled while requesting: %w", chamberErrors.ErrCanceled, label, ctxErr)
		}
		return "", nil, fmt.Errorf("%w: download %s: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%w: download %s: unexpected HTTP status %s", chamberErrors.ErrBuildInstallFailed, label, response.Status)
	}
	tempRel, ok := relBelow(b.workspace.Root(), tempDir)
	if !ok {
		return "", nil, fmt.Errorf("%w: %s temporary directory %q is outside image workspace", chamberErrors.ErrInvalidRequest, label, tempDir)
	}
	tmp, err := b.workspace.CreateTemp(tempRel, "."+strings.ToLower(label)+"-*.tar.gz")
	if err != nil {
		return "", nil, fmt.Errorf("%w: create temporary %s archive: %w", chamberErrors.ErrFilesystemFailed, label, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, digest), response.Body); err != nil {
		_ = tmp.Close()
		cleanup()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", nil, fmt.Errorf("%w: %s download canceled while reading response: %w", chamberErrors.ErrCanceled, label, ctxErr)
		}
		return "", nil, fmt.Errorf("%w: download %s: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	actualDigest := digest.Sum(nil)
	if !equalDigest(actualDigest, expectedDigest) {
		_ = tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("%w: verify %s checksum: got %s, want %s", chamberErrors.ErrBuildInstallFailed, label, hex.EncodeToString(actualDigest), hex.EncodeToString(expectedDigest))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("%w: sync %s archive: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%w: close %s archive: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	return tmpPath, cleanup, nil
}

func (b Builder) extractManagedArchive(archivePath string, expected map[string]string, label string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("%w: open %s archive: %w", chamberErrors.ErrFilesystemFailed, label, err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("%w: decompress %s archive: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	defer gzipReader.Close()

	seen := make(map[string]bool, len(expected))
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read %s archive: %w", chamberErrors.ErrBuildInstallFailed, label, err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		destination, ok := expected[header.Name]
		if !ok {
			return fmt.Errorf("%w: unexpected %s archive entry %q", chamberErrors.ErrBuildInstallFailed, label, header.Name)
		}
		if err := validateManagedArchiveHeader(header, label); err != nil {
			return err
		}
		if destination == "" {
			continue
		}
		if err := b.writeExecutableAtomic(destination, reader); err != nil {
			return err
		}
		seen[header.Name] = true
	}
	for name, destination := range expected {
		if destination != "" && !seen[name] {
			return fmt.Errorf("%w: %s archive missing expected entry %q", chamberErrors.ErrBuildInstallFailed, label, name)
		}
	}
	return nil
}

func validateManagedArchiveHeader(header *tar.Header, label string) error {
	if header == nil {
		return fmt.Errorf("%w: nil %s archive header", chamberErrors.ErrBuildInstallFailed, label)
	}
	clean := filepath.Clean(header.Name)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: unsafe %s archive path %q", chamberErrors.ErrBuildInstallFailed, label, header.Name)
	}
	if header.Linkname != "" {
		return fmt.Errorf("%w: unsupported %s archive link %q", chamberErrors.ErrBuildInstallFailed, label, header.Name)
	}
	switch header.Typeflag {
	case tar.TypeReg, 0:
		return nil
	default:
		return fmt.Errorf("%w: unsupported %s archive entry %q", chamberErrors.ErrBuildInstallFailed, label, header.Name)
	}
}

func (b Builder) downloadBinary(ctx context.Context, url string, expectedDigest []byte, binDir string, binaryPath string, label string) error {
	client := b.client
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: create %s download request: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %s download canceled while requesting: %w", chamberErrors.ErrCanceled, label, ctxErr)
		}
		return fmt.Errorf("%w: download %s: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: download %s: unexpected HTTP status %s", chamberErrors.ErrBuildInstallFailed, label, response.Status)
	}
	binRel, ok := relBelow(b.workspace.Root(), binDir)
	if !ok {
		return fmt.Errorf("%w: runtime binary directory %q is outside image workspace", chamberErrors.ErrInvalidRequest, binDir)
	}
	tmp, err := b.workspace.CreateTemp(binRel, "."+filepath.Base(binaryPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary %s binary: %w", chamberErrors.ErrFilesystemFailed, label, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, digest), response.Body); err != nil {
		_ = tmp.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %s download canceled while reading response: %w", chamberErrors.ErrCanceled, label, ctxErr)
		}
		return fmt.Errorf("%w: download %s: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	actualDigest := digest.Sum(nil)
	if !equalDigest(actualDigest, expectedDigest) {
		_ = tmp.Close()
		return fmt.Errorf("%w: verify %s checksum: got %s, want %s", chamberErrors.ErrBuildInstallFailed, label, hex.EncodeToString(actualDigest), hex.EncodeToString(expectedDigest))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: sync %s binary: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: set %s mode: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close %s binary: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		return fmt.Errorf("%w: commit %s binary: %w", chamberErrors.ErrBuildInstallFailed, label, err)
	}
	committed = true
	return nil
}

func probeTool(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("tool path is required")
	}
	command := exec.CommandContext(ctx, path, "--version")
	if output, err := command.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: probe canceled: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("version probe failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func parseSHA256(raw string, label string) ([]byte, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "sha256:")
	digest, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s sha256: %w", chamberErrors.ErrInvalidRequest, label, err)
	}
	if len(digest) != sha256.Size {
		return nil, fmt.Errorf("%w: parse %s sha256: got %d bytes, want %d", chamberErrors.ErrInvalidRequest, label, len(digest), sha256.Size)
	}
	return digest, nil
}

func fileMatchesSHA256(path string, expectedDigest []byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return false, err
	}
	return equalDigest(digest.Sum(nil), expectedDigest), nil
}

func equalDigest(actual []byte, expected []byte) bool {
	if len(actual) != len(expected) {
		return false
	}
	var diff byte
	for i := range actual {
		diff |= actual[i] ^ expected[i]
	}
	return diff == 0
}
