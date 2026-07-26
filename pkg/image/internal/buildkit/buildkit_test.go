package buildkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestResolveRequestDefaultsDockerfileAndNormalizesPlatform(t *testing.T) {
	contextPath := privateTempDir(t)
	dockerfilePath := filepath.Join(contextPath, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatalf("WriteFile(Dockerfile) error = %v", err)
	}

	resolved, err := resolveRequest(chamberImage.BuildRequest{
		Reference:   "example.com/library/app:latest",
		ContextPath: contextPath,
		BuildArgs: map[string]string{
			"BETA":  "2",
			"ALPHA": "1",
		},
	})
	if err != nil {
		t.Fatalf("resolveRequest() error = %v", err)
	}
	if resolved.reference != "example.com/library/app:latest" {
		t.Fatalf("reference = %q, want canonical input", resolved.reference)
	}
	resolvedDockerfilePath, err := filepath.EvalSymlinks(dockerfilePath)
	if err != nil {
		t.Fatalf("EvalSymlinks(Dockerfile) error = %v", err)
	}
	if resolved.dockerfilePath != resolvedDockerfilePath {
		t.Fatalf("dockerfilePath = %q, want %q", resolved.dockerfilePath, resolvedDockerfilePath)
	}
	if resolved.dockerfileRelPath != "Dockerfile" {
		t.Fatalf("dockerfileRelPath = %q, want Dockerfile", resolved.dockerfileRelPath)
	}
	if resolved.platform.OS == "" || resolved.platform.Architecture == "" {
		t.Fatalf("platform = %#v, want normalized platform", resolved.platform)
	}
	if !reflect.DeepEqual(resolved.buildArgs, map[string]string{"ALPHA": "1", "BETA": "2"}) {
		t.Fatalf("buildArgs = %#v, want request build args", resolved.buildArgs)
	}
}

func TestResolveRequestRejectsDockerfileSymlinkOutsideContext(t *testing.T) {
	contextPath := privateTempDir(t)
	outsidePath := filepath.Join(privateTempDir(t), "Dockerfile")
	if err := os.WriteFile(outsidePath, []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatalf("WriteFile(outside Dockerfile) error = %v", err)
	}
	linkPath := filepath.Join(contextPath, "Dockerfile")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("Symlink() error = %v", err)
	}

	if _, err := resolveRequest(chamberImage.BuildRequest{
		Reference:   "example.com/library/app:latest",
		ContextPath: contextPath,
	}); err == nil {
		t.Fatal("resolveRequest() error = nil, want symlink escape rejected")
	}
}

func TestResolveRequestRejectsEmptyBuildArgKey(t *testing.T) {
	contextPath := privateTempDir(t)
	if err := os.WriteFile(filepath.Join(contextPath, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatalf("WriteFile(Dockerfile) error = %v", err)
	}
	_, err := resolveRequest(chamberImage.BuildRequest{
		Reference:   "example.com/library/app:latest",
		ContextPath: contextPath,
		BuildArgs:   map[string]string{" ": "value"},
	})
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("resolveRequest() error = %v, want invalid request", err)
	}
}

func TestValidateConfigRejectsUnsupportedSnapshotterAndRelativePaths(t *testing.T) {
	if err := validateConfig(chamberImage.BuildKitConfig{Snapshotter: "overlayfs"}); !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("validateConfig(unsupported snapshotter) error = %v, want invalid request", err)
	}
	if err := validateConfig(chamberImage.BuildKitConfig{BuildctlPath: "buildctl", BuildkitdPath: "/usr/bin/buildkitd"}); !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("validateConfig(relative path) error = %v, want invalid request", err)
	}
}

func TestValidateRejectsRootThatWouldCreateOverlongBuildKitSocketPath(t *testing.T) {
	root := "/" + strings.Repeat("a", linuxPathnameSocketMaxBytes)
	err := (Builder{
		root:             root,
		directoryManager: localfs.NewDirectoryManager(),
	}).validate(context.Background())
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("validate() error = %v, want invalid request", err)
	}
	if !strings.Contains(err.Error(), "Unix socket path") {
		t.Fatalf("validate() error = %v, want socket path explanation", err)
	}
}

func TestLocalToolDoesNotChangeCallerOwnedPermissions(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "buildctl")
	if err := os.WriteFile(path, executableContent("buildctl"), 0700); err != nil {
		t.Fatalf("WriteFile(buildctl) error = %v", err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(buildctl) error = %v", err)
	}

	got, err := (Builder{}).localTool(context.Background(), "buildctl", path)
	if err != nil {
		t.Fatalf("localTool() error = %v", err)
	}
	if got != path {
		t.Fatalf("localTool() = %q, want %q", got, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(buildctl) error = %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0700 {
		t.Fatalf("mode = %o, want unchanged 0700", mode)
	}
}

func TestManagedArchivesRequireOverrideTriples(t *testing.T) {
	if _, err := buildKitArchive(chamberImage.BuildKitConfig{BuildKitVersion: "v1"}); !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("buildKitArchive(partial override) error = %v, want invalid request", err)
	}
	if _, err := rootlessKitArchive(chamberImage.BuildKitConfig{RootlessKitURL: "https://example.test/rootlesskit.tar.gz"}); !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("rootlessKitArchive(partial override) error = %v, want invalid request", err)
	}
}

func TestManagedArchiveDefaultsMapArchitectures(t *testing.T) {
	withHost(t, "linux", "amd64", 1000)
	buildKit, err := buildKitArchive(chamberImage.BuildKitConfig{})
	if err != nil {
		t.Fatalf("buildKitArchive(amd64) error = %v", err)
	}
	if buildKit.url != defaultBuildKitAMD64URL || buildKit.sha256 != defaultBuildKitAMD64SHA256 {
		t.Fatalf("buildKit amd64 = %#v, want default amd64 URL/checksum", buildKit)
	}
	rootlessKit, err := rootlessKitArchive(chamberImage.BuildKitConfig{})
	if err != nil {
		t.Fatalf("rootlessKitArchive(amd64) error = %v", err)
	}
	if rootlessKit.url != defaultRootlessKitAMD64URL || rootlessKit.sha256 != defaultRootlessKitAMD64SHA256 {
		t.Fatalf("rootlessKit amd64 = %#v, want default amd64 URL/checksum", rootlessKit)
	}

	withHost(t, "linux", "arm64", 1000)
	buildKit, err = buildKitArchive(chamberImage.BuildKitConfig{})
	if err != nil {
		t.Fatalf("buildKitArchive(arm64) error = %v", err)
	}
	if buildKit.url != defaultBuildKitARM64URL || buildKit.sha256 != defaultBuildKitARM64SHA256 {
		t.Fatalf("buildKit arm64 = %#v, want default arm64 URL/checksum", buildKit)
	}
	rootlessKit, err = rootlessKitArchive(chamberImage.BuildKitConfig{})
	if err != nil {
		t.Fatalf("rootlessKitArchive(arm64) error = %v", err)
	}
	if rootlessKit.url != defaultRootlessKitARM64URL || rootlessKit.sha256 != defaultRootlessKitARM64SHA256 {
		t.Fatalf("rootlessKit arm64 = %#v, want default arm64 URL/checksum", rootlessKit)
	}
}

func TestDownloadAndExtractArchiveInstallsOnlyExpectedExecutables(t *testing.T) {
	withHost(t, "linux", "amd64", 1000)
	root := privateTempDir(t)
	binDir := filepath.Join(root, "bin")
	directoryManager := localfs.NewDirectoryManager()
	if err := directoryManager.MkdirPrivate(binDir); err != nil {
		t.Fatalf("MkdirPrivate(bin) error = %v", err)
	}
	content := gzipTar(t, map[string][]byte{
		"bin/buildctl":  executableContent("buildctl"),
		"bin/buildkitd": executableContent("buildkitd"),
	})
	builder := Builder{
		client:           responseClient(http.StatusOK, io.NopCloser(bytes.NewReader(content))),
		directoryManager: directoryManager,
	}
	err := builder.downloadAndExtractArchive(context.Background(), managedArchive{
		version: "test",
		url:     "https://example.test/buildkit.tar.gz",
		sha256:  sha256Hex(content),
	}, binDir, map[string]string{
		"bin/buildctl":  filepath.Join(binDir, "buildctl"),
		"bin/buildkitd": filepath.Join(binDir, "buildkitd"),
	}, "BuildKit")
	if err != nil {
		t.Fatalf("downloadAndExtractArchive() error = %v", err)
	}
	assertFileContentAndMode(t, filepath.Join(binDir, "buildctl"), executableContent("buildctl"), 0755)
	assertFileContentAndMode(t, filepath.Join(binDir, "buildkitd"), executableContent("buildkitd"), 0755)
}

func TestDownloadAndExtractArchiveAllowsOfficialUnextractedHelpers(t *testing.T) {
	root := privateTempDir(t)
	content := gzipTar(t, map[string][]byte{
		"bin/buildctl":              executableContent("buildctl"),
		"bin/buildkit-cni-bridge":   executableContent("bridge"),
		"bin/buildkit-qemu-aarch64": executableContent("qemu"),
		"bin/buildkit-qemu-x86_64":  executableContent("qemu"),
		"bin/buildkit-runc":         executableContent("buildkit-runc"),
		"bin/buildkitd":             executableContent("buildkitd"),
	})
	builder := Builder{
		client:           responseClient(http.StatusOK, io.NopCloser(bytes.NewReader(content))),
		directoryManager: localfs.NewDirectoryManager(),
	}
	err := builder.downloadAndExtractArchive(context.Background(), managedArchive{
		version: "test",
		url:     "https://example.test/buildkit.tar.gz",
		sha256:  sha256Hex(content),
	}, root, map[string]string{
		"bin/buildctl":              filepath.Join(root, "buildctl"),
		"bin/buildkit-cni-bridge":   "",
		"bin/buildkit-qemu-aarch64": "",
		"bin/buildkit-qemu-x86_64":  "",
		"bin/buildkit-runc":         "",
		"bin/buildkitd":             filepath.Join(root, "buildkitd"),
	}, "BuildKit")
	if err != nil {
		t.Fatalf("downloadAndExtractArchive() error = %v", err)
	}
	assertFileContentAndMode(t, filepath.Join(root, "buildctl"), executableContent("buildctl"), 0755)
	assertFileContentAndMode(t, filepath.Join(root, "buildkitd"), executableContent("buildkitd"), 0755)
	for _, name := range []string{"buildkit-cni-bridge", "buildkit-qemu-aarch64", "buildkit-qemu-x86_64", "buildkit-runc"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("Stat(%s) error = %v, want helper not extracted", name, err)
		}
	}
}

func TestDownloadAndExtractArchiveRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
	}{
		{name: "absolute", entries: []tarEntry{{name: "/bin/buildctl", content: []byte("bad")}}},
		{name: "dotdot", entries: []tarEntry{{name: "../buildctl", content: []byte("bad")}}},
		{name: "symlink", entries: []tarEntry{{name: "bin/buildctl", linkname: "target", typeflag: tar.TypeSymlink}}},
		{name: "hardlink", entries: []tarEntry{{name: "bin/buildctl", linkname: "target", typeflag: tar.TypeLink}}},
		{name: "device", entries: []tarEntry{{name: "bin/buildctl", typeflag: tar.TypeChar}}},
		{name: "unexpected", entries: []tarEntry{{name: "bin/extra", content: []byte("bad")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := privateTempDir(t)
			content := gzipTarEntries(t, tt.entries)
			builder := Builder{
				client:           responseClient(http.StatusOK, io.NopCloser(bytes.NewReader(content))),
				directoryManager: localfs.NewDirectoryManager(),
			}
			err := builder.downloadAndExtractArchive(context.Background(), managedArchive{
				version: "test",
				url:     "https://example.test/buildkit.tar.gz",
				sha256:  sha256Hex(content),
			}, root, map[string]string{"bin/buildctl": filepath.Join(root, "buildctl")}, "BuildKit")
			if !errors.Is(err, chamberErrors.ErrBuildInstallFailed) {
				t.Fatalf("downloadAndExtractArchive() error = %v, want build install failed", err)
			}
		})
	}
}

func TestDownloadAndExtractArchiveRejectsWrongChecksum(t *testing.T) {
	content := gzipTar(t, map[string][]byte{"bin/buildctl": executableContent("buildctl")})
	builder := Builder{
		client:           responseClient(http.StatusOK, io.NopCloser(bytes.NewReader(content))),
		directoryManager: localfs.NewDirectoryManager(),
	}
	err := builder.downloadAndExtractArchive(context.Background(), managedArchive{
		version: "test",
		url:     "https://example.test/buildkit.tar.gz",
		sha256:  sha256Hex([]byte("different")),
	}, privateTempDir(t), map[string]string{"bin/buildctl": filepath.Join(privateTempDir(t), "buildctl")}, "BuildKit")
	if !errors.Is(err, chamberErrors.ErrBuildInstallFailed) {
		t.Fatalf("downloadAndExtractArchive() error = %v, want build install failed", err)
	}
}

func TestBuildCommandsUseChamberOwnedStateAndEnvironment(t *testing.T) {
	root := privateTempDir(t)
	builder := Builder{
		root: root,
		paths: toolPaths{
			buildctl:    "/bin/buildctl",
			buildkitd:   "/bin/buildkitd",
			rootlesskit: "/bin/rootlesskit",
			runc:        "/bin/runc",
		},
		snapshotter: defaultSnapshotter,
	}
	dirs := buildDirectories{
		stateDir:      filepath.Join(root, "tmp", ".buildkit-state-1"),
		runDir:        filepath.Join(root, "tmp", ".buildkit-run-1"),
		homeDir:       filepath.Join(root, "tmp", ".buildkit-home-1"),
		tmpDir:        filepath.Join(root, "tmp", ".buildkit-tmp-1"),
		dockerConfig:  filepath.Join(root, "tmp", ".buildkit-home-1", ".docker"),
		outputArchive: filepath.Join(root, "tmp", ".buildkit-output-1.tar"),
	}
	build := resolvedRequest{
		contextPath:       root,
		dockerfileRelPath: "build/Dockerfile",
		platform:          chamberImage.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		target:            "release",
		buildArgs:         map[string]string{"BETA": "2", "ALPHA": "1"},
	}
	daemonCommand, err := builder.buildkitdCommand(context.Background(), dirs)
	if err != nil {
		t.Fatalf("buildkitdCommand() error = %v", err)
	}
	wantDaemonArgs := []string{
		"/bin/rootlesskit",
		"--state-dir", dirs.runDir,
		"--net=host",
		"/bin/buildkitd",
		"--addr", "unix://" + filepath.Join(dirs.runDir, "buildkitd.sock"),
		"--root", dirs.stateDir,
		"--oci-worker=true",
		"--containerd-worker=false",
		"--oci-worker-binary", "/bin/runc",
		"--oci-worker-snapshotter", "native",
	}
	if !reflect.DeepEqual(daemonCommand.Args, wantDaemonArgs) {
		t.Fatalf("buildkitd args = %#v, want %#v", daemonCommand.Args, wantDaemonArgs)
	}

	buildCommand, err := builder.buildctlBuildCommand(context.Background(), build, dirs)
	if err != nil {
		t.Fatalf("buildctlBuildCommand() error = %v", err)
	}
	wantBuildArgs := []string{
		"/bin/buildctl",
		"--addr", "unix://" + filepath.Join(dirs.runDir, "buildkitd.sock"),
		"build",
		"--progress", "plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + root,
		"--local", "dockerfile=" + root,
		"--opt", "filename=build/Dockerfile",
		"--opt", "platform=linux/arm64/v8",
		"--output", "type=oci,dest=" + dirs.outputArchive,
		"--opt", "target=release",
		"--opt", "build-arg:ALPHA=1",
		"--opt", "build-arg:BETA=2",
	}
	if !reflect.DeepEqual(buildCommand.Args, wantBuildArgs) {
		t.Fatalf("buildctl args = %#v, want %#v", buildCommand.Args, wantBuildArgs)
	}
	for _, want := range []string{
		"XDG_RUNTIME_DIR=" + dirs.runDir,
		"TMPDIR=" + dirs.tmpDir,
		"HOME=" + dirs.homeDir,
		"DOCKER_CONFIG=" + dirs.dockerConfig,
	} {
		if !envContains(buildCommand.Env, want) {
			t.Fatalf("buildctl Env missing %q in %#v", want, buildCommand.Env)
		}
	}
}

func TestProcessOutputBufferCapturesAndLogsLines(t *testing.T) {
	var logs bytes.Buffer
	logger := chamberLogging.NewJSONLogger(&logs, nil)
	captured := &lockedBuffer{}
	writer := newProcessOutputBuffer(context.Background(), logger, "buildctl", "stderr", captured)

	if _, err := writer.Write([]byte("first line\npartial")); err != nil {
		t.Fatalf("Write(first) error = %v", err)
	}
	if _, err := writer.Write([]byte(" line\n")); err != nil {
		t.Fatalf("Write(second) error = %v", err)
	}
	writer.Flush()

	if got := captured.String(); got != "first line\npartial line\n" {
		t.Fatalf("captured output = %q, want complete original output", got)
	}
	output := logs.String()
	for _, want := range []string{
		`"msg":"buildkit process output"`,
		`"process":"buildctl"`,
		`"stream":"stderr"`,
		`"line":"first line"`,
		`"line":"partial line"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs missing %s in %s", want, output)
		}
	}

	writer.SuppressLogs()
	if _, err := writer.Write([]byte("shutdown noise\n")); err != nil {
		t.Fatalf("Write(suppressed) error = %v", err)
	}
	writer.Flush()
	if !strings.Contains(captured.String(), "shutdown noise\n") {
		t.Fatalf("captured output after suppression = %q, want suppressed line captured", captured.String())
	}
	if strings.Contains(logs.String(), "shutdown noise") {
		t.Fatalf("logs contain suppressed shutdown line: %s", logs.String())
	}
}

func TestExtractOCITarWritesLayout(t *testing.T) {
	tarPath := filepath.Join(privateTempDir(t), "layout.tar")
	if err := writeValidOCITar(tarPath); err != nil {
		t.Fatalf("writeValidOCITar() error = %v", err)
	}
	outputLayout := filepath.Join(privateTempDir(t), "layout")
	directoryManager := localfs.NewDirectoryManager()
	if err := directoryManager.MkdirPrivate(outputLayout); err != nil {
		t.Fatalf("MkdirPrivate(output layout) error = %v", err)
	}

	builder := Builder{directoryManager: directoryManager}
	if err := builder.extractOCITar(tarPath, outputLayout); err != nil {
		t.Fatalf("extractOCITar() error = %v", err)
	}
	if !chamberImage.LayoutExists(outputLayout) {
		t.Fatal("output layout is not a valid OCI image layout")
	}
}

func writeValidOCITar(path string) error {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	config := testDescriptor(imagespec.MediaTypeImageConfig, []byte(`{"architecture":"arm64","os":"linux"}`))
	layer := testDescriptor("application/vnd.oci.image.layer.v1.tar", []byte("layer"))
	manifestBytes, err := json.Marshal(imagespec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Config:    config,
		Layers:    []imagespec.Descriptor{layer},
	})
	if err != nil {
		return err
	}
	manifest := testDescriptor(imagespec.MediaTypeImageManifest, manifestBytes)
	indexBytes, err := json.Marshal(imagespec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Manifests: []imagespec.Descriptor{
			{
				MediaType: manifest.MediaType,
				Digest:    manifest.Digest,
				Size:      manifest.Size,
				Platform:  &imagespec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
			},
		},
	})
	if err != nil {
		return err
	}
	entries := map[string][]byte{
		"oci-layout":       []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":       indexBytes,
		blobName(config):   []byte(`{"architecture":"arm64","os":"linux"}`),
		blobName(layer):    []byte("layer"),
		blobName(manifest): manifestBytes,
	}
	for name, content := range entries {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(content))}); err != nil {
			return err
		}
		if _, err := writer.Write(content); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0600)
}

func testDescriptor(mediaType string, content []byte) imagespec.Descriptor {
	sum := sha256.Sum256(content)
	return imagespec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.Digest("sha256:" + hex.EncodeToString(sum[:])),
		Size:      int64(len(content)),
	}
}

func blobName(descriptor imagespec.Descriptor) string {
	return filepath.Join("blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded())
}

func withHost(t *testing.T, goos string, arch string, uid int) {
	t.Helper()
	originalGOOS := runtimeGOOS
	originalArch := runtimeArch
	originalUID := currentUID
	runtimeGOOS = func() string { return goos }
	runtimeArch = func() string { return arch }
	currentUID = func() int { return uid }
	t.Cleanup(func() {
		runtimeGOOS = originalGOOS
		runtimeArch = originalArch
		currentUID = originalUID
	})
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func executableContent(name string) []byte {
	return []byte("#!/bin/sh\nprintf '" + name + " version test\\n'\n")
}

type tarEntry struct {
	name     string
	content  []byte
	linkname string
	typeflag byte
}

func gzipTar(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	tarEntries := make([]tarEntry, 0, len(entries))
	for name, content := range entries {
		tarEntries = append(tarEntries, tarEntry{name: name, content: content})
	}
	return gzipTarEntries(t, tarEntries)
}

func gzipTarEntries(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0600,
			Size:     int64(len(entry.content)),
			Linkname: entry.linkname,
			Typeflag: typeflag,
		}
		if typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader(%q) error = %v", entry.name, err)
		}
		if header.Size > 0 {
			if _, err := tarWriter.Write(entry.content); err != nil {
				t.Fatalf("Write(%q) error = %v", entry.name, err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar Close() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}
	return buf.Bytes()
}

func envContains(env []string, value string) bool {
	for _, item := range env {
		if item == value {
			return true
		}
	}
	return false
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func assertFileContentAndMode(t *testing.T, path string, wantContent []byte, wantMode os.FileMode) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if !bytes.Equal(content, wantContent) {
		t.Fatalf("ReadFile(%q) = %q, want %q", path, string(content), string(wantContent))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != wantMode {
		t.Fatalf("mode(%q) = %o, want %o", path, got, wantMode)
	}
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (fn httpClientFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func responseClient(statusCode int, body io.ReadCloser) *http.Client {
	return &http.Client{Transport: httpClientFunc(func(*http.Request) (*http.Response, error) {
		return response(statusCode, body), nil
	})}
}

func response(statusCode int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Body:       body,
		Header:     make(http.Header),
	}
}
