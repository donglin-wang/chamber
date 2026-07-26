package buildah

import (
	"archive/tar"
	"bytes"
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

func TestBuildCommandUsesChamberOwnedRootsAndTemporaryDirectory(t *testing.T) {
	root := privateTempDir(t)
	workerPath := executableFile(t)
	builder, err := New(context.Background(), chamberImage.BuildahConfig{
		Path:          workerPath,
		SHA256:        fileSHA256Hex(t, workerPath),
		Runtime:       "/usr/bin/crun",
		Isolation:     "chroot",
		StorageDriver: "vfs",
	}, root, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	command, err := builder.buildWorkerCommand(context.Background(), resolvedRequest{
		reference:      "example.com/library/app:latest",
		contextPath:    root,
		dockerfilePath: filepath.Join(root, "Dockerfile"),
		platform:       chamberImage.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"},
		target:         "release",
		buildArgs: map[string]string{
			"BETA":  "2",
			"ALPHA": "1",
		},
	}, filepath.Join(root, "out.tar"))
	if err != nil {
		t.Fatalf("buildWorkerCommand() error = %v", err)
	}
	if command.Path != workerPath {
		t.Fatalf("command.Path = %q, want %q", command.Path, workerPath)
	}
	wantArgs := []string{workerPath}
	if !reflect.DeepEqual(command.Args, wantArgs) {
		t.Fatalf("command.Args = %#v, want %#v", command.Args, wantArgs)
	}
	gotRequest := readWorkerRequest(t, command.Stdin)
	wantRequest := workerBuildRequest{
		ProtocolVersion: protocolVersion,
		Operation:       "build",
		Reference:       "example.com/library/app:latest",
		ContextPath:     root,
		DockerfilePath:  filepath.Join(root, "Dockerfile"),
		OutputArchive:   filepath.Join(root, "out.tar"),
		GraphRoot:       filepath.Join(root, "buildah-root"),
		RunRoot:         filepath.Join(root, "buildah-runroot"),
		StorageDriver:   "vfs",
		Runtime:         "/usr/bin/crun",
		Isolation:       "chroot",
		Platform:        chamberImage.Platform{OS: "linux", Architecture: "amd64", Variant: "v3"},
		Target:          "release",
		BuildArgs: map[string]string{
			"ALPHA": "1",
			"BETA":  "2",
		},
	}
	if !reflect.DeepEqual(gotRequest, wantRequest) {
		t.Fatalf("worker request = %#v, want %#v", gotRequest, wantRequest)
	}
	if command.Dir != root {
		t.Fatalf("command.Dir = %q, want %q", command.Dir, root)
	}
	if !envContains(command.Env, "TMPDIR="+filepath.Join(root, "buildah-tmp")) {
		t.Fatalf("command.Env does not contain Chamber TMPDIR: %#v", command.Env)
	}
	if !envContains(command.Env, "XDG_RUNTIME_DIR="+filepath.Join(root, "buildah-runroot")) {
		t.Fatalf("command.Env does not contain Chamber XDG_RUNTIME_DIR: %#v", command.Env)
	}
}

func TestNewPreparesManagedWorkerAndBuildahRoots(t *testing.T) {
	content := validWorkerContent()
	root := privateTempDir(t)
	workerPath := filepath.Join(root, "bin", "buildah-worker")
	if err := localfs.NewDirectoryManager().MkdirPrivate(filepath.Dir(workerPath)); err != nil {
		t.Fatalf("MkdirPrivate(bin) error = %v", err)
	}
	if err := os.WriteFile(workerPath, content, 0600); err != nil {
		t.Fatalf("WriteFile(buildah-worker) error = %v", err)
	}

	builder, err := New(context.Background(), chamberImage.BuildahConfig{
		Version: "test-version",
		URL:     "https://example.test/buildah-worker",
		SHA256:  sha256Hex(content),
	}, root, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if builder.workerPath != workerPath {
		t.Fatalf("workerPath = %q, want %q", builder.workerPath, workerPath)
	}
	for _, path := range []string{
		filepath.Join(root, "buildah-root"),
		filepath.Join(root, "buildah-runroot"),
		filepath.Join(root, "buildah-tmp"),
	} {
		assertPrivateDirectory(t, path)
	}
	assertFileContentAndMode(t, workerPath, content, 0755)
}

func TestWorkerExecutableRejectsRelativeLocalPath(t *testing.T) {
	if _, err := New(context.Background(), chamberImage.BuildahConfig{Path: "buildah"}, privateTempDir(t), localfs.NewDirectoryManager()); err == nil {
		t.Fatal("New() error = nil, want relative path rejected")
	}
}

func TestDefaultManagedWorkerPathBelowImageRoot(t *testing.T) {
	root := privateTempDir(t)
	if got := defaultManagedWorkerPath(root); got != filepath.Join(root, "bin", "buildah-worker") {
		t.Fatalf("defaultManagedWorkerPath() = %q, want default below image root", got)
	}
}

func TestManagedBinaryDefaultsToBeta4WorkerURL(t *testing.T) {
	originalRuntimeArch := runtimeArch
	defer func() { runtimeArch = originalRuntimeArch }()

	tests := []struct {
		name   string
		arch   string
		url    string
		sha256 string
	}{
		{
			name:   "amd64",
			arch:   "amd64",
			url:    "https://github.com/donglin-wang/chamber/releases/download/v0.1.0-beta.4/buildah-worker-linux-amd64",
			sha256: "649dea3351658956bbca0a635baca721968f4cb37b20db8a344d817ac7415bcd",
		},
		{
			name:   "arm64",
			arch:   "arm64",
			url:    "https://github.com/donglin-wang/chamber/releases/download/v0.1.0-beta.4/buildah-worker-linux-arm64",
			sha256: "62ec2157a1783aad5fdb9916774c424729786e8827c22ce8acbd268a7fa10402",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtimeArch = func() string { return tt.arch }

			binary, err := managedBinary(chamberImage.BuildahConfig{})
			if err != nil {
				t.Fatalf("managedBinary() error = %v", err)
			}
			if binary.version != "v0.1.0-beta.4" {
				t.Fatalf("version = %q, want v0.1.0-beta.4", binary.version)
			}
			if binary.url != tt.url {
				t.Fatalf("url = %q, want beta.4 %s worker URL", binary.url, tt.arch)
			}
			if binary.sha256 != tt.sha256 {
				t.Fatalf("sha256 = %q, want current beta.4 %s worker checksum", binary.sha256, tt.arch)
			}
		})
	}
}

func TestWorkerExecutableDownloadsValidBinary(t *testing.T) {
	content := validWorkerContent()
	root := privateTempDir(t)
	workerPath := filepath.Join(root, "bin", "buildah-worker")
	builder := Builder{
		config: chamberImage.BuildahConfig{
			Version: "test-version",
			URL:     "https://example.test/buildah-worker",
			SHA256:  sha256Hex(content),
		},
		root:             root,
		client:           responseClient(http.StatusOK, io.NopCloser(bytes.NewReader(content))),
		directoryManager: localfs.NewDirectoryManager(),
	}
	if err := builder.prepare(context.Background()); err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	got := builder.workerPath
	if got != workerPath {
		t.Fatalf("workerPath = %q, want %q", got, workerPath)
	}
	assertFileContentAndMode(t, workerPath, content, 0755)
}

func TestWorkerExecutableReusesExistingValidBinary(t *testing.T) {
	content := validWorkerContent()
	root := privateTempDir(t)
	workerPath := filepath.Join(root, "bin", "buildah-worker")
	if err := localfs.NewDirectoryManager().MkdirPrivate(filepath.Dir(workerPath)); err != nil {
		t.Fatalf("MkdirPrivate(bin) error = %v", err)
	}
	if err := os.WriteFile(workerPath, content, 0600); err != nil {
		t.Fatalf("WriteFile(buildah-worker) error = %v", err)
	}
	requests := 0
	client := &http.Client{Transport: httpClientFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusOK, io.NopCloser(strings.NewReader("unexpected"))), nil
	})}
	builder := Builder{
		config: chamberImage.BuildahConfig{
			Version: "test-version",
			URL:     "https://example.test/buildah-worker",
			SHA256:  sha256Hex(content),
		},
		root:             root,
		client:           client,
		directoryManager: localfs.NewDirectoryManager(),
	}
	if err := builder.prepare(context.Background()); err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	got := builder.workerPath
	if got != workerPath {
		t.Fatalf("workerPath = %q, want %q", got, workerPath)
	}
	if requests != 0 {
		t.Fatalf("download requests = %d, want 0", requests)
	}
	assertFileContentAndMode(t, workerPath, content, 0755)
}

func TestWorkerExecutableRejectsWrongDigest(t *testing.T) {
	content := []byte("not the pinned binary")
	root := privateTempDir(t)
	workerPath := filepath.Join(root, "bin", "buildah-worker")
	builder := Builder{
		config: chamberImage.BuildahConfig{
			Version: "test-version",
			URL:     "https://example.test/buildah-worker",
			SHA256:  sha256Hex([]byte("expected binary")),
		},
		root:             root,
		client:           responseClient(http.StatusOK, io.NopCloser(bytes.NewReader(content))),
		directoryManager: localfs.NewDirectoryManager(),
	}
	err := builder.prepare(context.Background())
	if err == nil {
		t.Fatal("prepare() error = nil, want digest error")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("prepare() error = %v, want checksum failure", err)
	}
	if !errors.Is(err, chamberErrors.ErrBuildInstallFailed) {
		t.Fatalf("prepare() error = %v, want build install failed code", err)
	}
	if _, statErr := os.Stat(workerPath); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestWorkerExecutableUsesConfiguredLocalWorker(t *testing.T) {
	root := privateTempDir(t)
	workerPath := filepath.Join(privateTempDir(t), "buildah-worker")
	content := validWorkerContent()
	if err := os.WriteFile(workerPath, content, 0600); err != nil {
		t.Fatalf("WriteFile(buildah-worker) error = %v", err)
	}
	requests := 0
	client := &http.Client{Transport: httpClientFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusOK, io.NopCloser(strings.NewReader("unexpected"))), nil
	})}
	builder := Builder{
		config:           chamberImage.BuildahConfig{Path: workerPath},
		root:             root,
		client:           client,
		directoryManager: localfs.NewDirectoryManager(),
	}
	if err := builder.prepare(context.Background()); err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	got := builder.workerPath
	if got != workerPath {
		t.Fatalf("workerPath = %q, want %q", got, workerPath)
	}
	if requests != 0 {
		t.Fatalf("download requests = %d, want 0", requests)
	}
	assertFileContentAndMode(t, workerPath, content, 0755)
}

func TestWorkerExecutableRejectsMissingConfiguredLocalWorker(t *testing.T) {
	root := privateTempDir(t)
	workerPath := filepath.Join(root, "missing-buildah-worker")
	_, err := New(context.Background(), chamberImage.BuildahConfig{Path: workerPath}, root, localfs.NewDirectoryManager())
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("New() error = %v, want invalid request", err)
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

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func executableFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(privateTempDir(t), "buildah-worker")
	if err := os.WriteFile(path, validWorkerContent(), 0700); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}
	return path
}

func validWorkerContent() []byte {
	return []byte("#!/bin/sh\nprintf '{\"protocol_version\":1}\\n'\n")
}

func readWorkerRequest(t *testing.T, reader io.Reader) workerBuildRequest {
	t.Helper()

	var request workerBuildRequest
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		t.Fatalf("Decode(worker request) error = %v", err)
	}
	return request
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

func fileSHA256Hex(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return sha256Hex(content)
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

func assertPrivateDirectory(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Fatalf("mode(%q) = %o, want 700", path, got)
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
