package runc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestNewPreparesRuntimeDirectories(t *testing.T) {
	root := filepath.Join(privateTempDir(t), "runtime")
	binDir := filepath.Join(privateTempDir(t), "bin")
	content := []byte("binary")

	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   root,
		RuntimeBinDir: binDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(content))))), withTestArtifact(content))

	if runtime == nil {
		t.Fatal("New() runtime = nil, want runtime")
	}
	assertPrivateDir(t, root)
	assertPrivateDir(t, binDir)
}

func TestNewRequiresDirectoryManager(t *testing.T) {
	_, err := New(context.Background(), chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
	}, nil)
	if err == nil {
		t.Fatal("New() error = nil, want directory manager error")
	}
}

func TestNewDownloadsValidRuntimeBinary(t *testing.T) {
	content := []byte("valid runc")
	binDir := privateTempDir(t)
	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(content))))), withTestArtifact(content))

	binary := runtime.Binary()
	if binary.Name != "runc" {
		t.Fatalf("Binary.Name = %q, want runc", binary.Name)
	}
	if binary.Path != filepath.Join(binDir, "runc") {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, filepath.Join(binDir, "runc"))
	}
	descriptor := runtime.Descriptor()
	if descriptor.Version != "test-version" {
		t.Fatalf("Descriptor().Version = %q, want test-version", descriptor.Version)
	}
	assertFileContentAndMode(t, binary.Path, content, 0755)
}

func TestNewDefaultsToRuncAdapterName(t *testing.T) {
	content := []byte("valid runc")
	binDir := privateTempDir(t)
	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(content))))), withTestArtifact(content))

	binary := runtime.Binary()
	if binary.Name != "runc" {
		t.Fatalf("Binary.Name = %q, want runc", binary.Name)
	}
	if binary.Path != filepath.Join(binDir, "runc") {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, filepath.Join(binDir, "runc"))
	}
}

func TestNewRejectsWrongDigest(t *testing.T) {
	content := []byte("not the pinned binary")
	binDir := privateTempDir(t)
	_, err := newWithOptions(context.Background(), chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(content))))), withTestArtifact([]byte("expected binary")))

	if err == nil {
		t.Fatal("New() error = nil, want digest error")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("New() error = %v, want checksum failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "runc")); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestNewRejectsNonOKResponse(t *testing.T) {
	_, err := newWithOptions(context.Background(), chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusNotFound, io.NopCloser(strings.NewReader("not found")))), withTestArtifact([]byte("anything")))

	if err == nil {
		t.Fatal("New() error = nil, want HTTP status error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("New() error = %v, want HTTP 404", err)
	}
}

func TestNewRejectsInterruptedBody(t *testing.T) {
	content := []byte("partial")
	binDir := privateTempDir(t)
	_, err := newWithOptions(context.Background(), chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, &interruptedBody{content: content})), withTestArtifact(content))

	if err == nil {
		t.Fatal("New() error = nil, want interrupted body error")
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "runc")); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestNewUsesExistingValidBinary(t *testing.T) {
	content := []byte("already cached")
	binDir := privateTempDir(t)
	path := filepath.Join(binDir, "runc")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	requests := 0
	client := &http.Client{Transport: httpClientFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusOK, io.NopCloser(strings.NewReader(""))), nil
	})}

	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(client), withTestArtifact(content))

	binary := runtime.Binary()
	if binary.Path != path {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, path)
	}
	if requests != 0 {
		t.Fatalf("download requests = %d, want 0", requests)
	}
	assertFileContentAndMode(t, path, content, 0755)
}

func TestNewMakesExistingValidBinaryExecutable(t *testing.T) {
	content := []byte("cached without executable mode")
	binDir := privateTempDir(t)
	path := filepath.Join(binDir, "runc")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	requests := 0
	client := &http.Client{Transport: httpClientFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusOK, io.NopCloser(strings.NewReader(""))), nil
	})}

	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(client), withTestArtifact(content))

	binary := runtime.Binary()
	if binary.Path != path {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, path)
	}
	if requests != 0 {
		t.Fatalf("download requests = %d, want 0", requests)
	}
	assertFileContentAndMode(t, path, content, 0755)
}

func TestNewReplacesExistingInvalidBinary(t *testing.T) {
	oldContent := []byte("corrupt cached binary")
	newContent := []byte("replacement binary")

	binDir := privateTempDir(t)
	path := filepath.Join(binDir, "runc")
	if err := os.WriteFile(path, oldContent, 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(newContent))))), withTestArtifact(newContent))

	binary := runtime.Binary()
	if binary.Path != path {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, path)
	}
	assertFileContentAndMode(t, path, newContent, 0755)
}

func TestDescriptorDeclaresRuncSupport(t *testing.T) {
	runtime := &Runtime{
		binary: chamberRuntimeShared.Binary{
			Name: "runc",
			Path: "/tmp/runc",
		},
		artifact: runtimeArtifact{
			version: "test-version",
		},
	}

	descriptor := runtime.Descriptor()

	if descriptor.Name != "runc" {
		t.Fatalf("Descriptor().Name = %q, want runc", descriptor.Name)
	}
	if descriptor.Version != "test-version" {
		t.Fatalf("Descriptor().Version = %q, want test-version", descriptor.Version)
	}
	if !slices.Equal(descriptor.Capabilities.Privileges, []capability.Privilege{capability.Rootless}) {
		t.Fatalf("privileges = %#v, want rootless only", descriptor.Capabilities.Privileges)
	}
	if !slices.Equal(descriptor.Capabilities.Isolation, []chamberRuntimeShared.Isolation{chamberRuntimeShared.ProcessIsolation}) {
		t.Fatalf("isolation = %#v, want process isolation", descriptor.Capabilities.Isolation)
	}
}

func TestDescriptorDefaultsToRuncAdapterName(t *testing.T) {
	descriptor := (&Runtime{}).Descriptor()

	if descriptor.Name != "runc" {
		t.Fatalf("Descriptor().Name = %q, want runc", descriptor.Name)
	}
	if descriptor.Version != defaultVersion {
		t.Fatalf("Descriptor().Version = %q, want %q", descriptor.Version, defaultVersion)
	}
}

func TestDescriptorNameDoesNotFollowConfiguredBinaryName(t *testing.T) {
	runtime := &Runtime{
		binary: chamberRuntimeShared.Binary{
			Name: "custom-runc-binary",
			Path: "/tmp/custom-runc-binary",
		},
		artifact: runtimeArtifact{
			version: "test-version",
		},
	}

	descriptor := runtime.Descriptor()

	if descriptor.Name != "runc" {
		t.Fatalf("Descriptor().Name = %q, want runc adapter name", descriptor.Name)
	}
	if descriptor.Version != "test-version" {
		t.Fatalf("Descriptor().Version = %q, want configured binary version", descriptor.Version)
	}
}

func TestNewReturnsAbsoluteBinaryPath(t *testing.T) {
	content := []byte("absolute")
	relativeBinDir := filepath.Join(".", t.Name())
	relativeRuntimeRoot := filepath.Join(".", t.Name()+"-state")
	t.Cleanup(func() {
		_ = os.RemoveAll(relativeBinDir)
		_ = os.RemoveAll(relativeRuntimeRoot)
	})

	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   relativeRuntimeRoot,
		RuntimeBinDir: relativeBinDir,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(content))))), withTestArtifact(content))

	binary := runtime.Binary()
	if !filepath.IsAbs(binary.Path) {
		t.Fatalf("Binary.Path = %q, want absolute path", binary.Path)
	}
}

func TestNewRequiresCompleteRuntimeArtifactConfiguration(t *testing.T) {
	_, err := newWithOptions(context.Background(), chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withArtifact(runtimeArtifact{
		version: "test-version",
		url:     "http://example.test/runc",
	}))

	if err == nil {
		t.Fatal("New() error = nil, want configuration error")
	}
}

func TestDefaultRuntimeArtifactSupportsLinuxReleaseArchitectures(t *testing.T) {
	tests := map[string]struct {
		url    string
		sha256 string
	}{
		"amd64": {
			url:    defaultAMD64URL,
			sha256: defaultAMD64SHA256,
		},
		"arm64": {
			url:    defaultARM64URL,
			sha256: defaultARM64SHA256,
		},
	}
	for arch, want := range tests {
		t.Run(arch, func(t *testing.T) {
			artifact, err := defaultRuntimeArtifact(arch)
			if err != nil {
				t.Fatalf("defaultRuntimeArtifact() error = %v", err)
			}
			if artifact.version != defaultVersion {
				t.Fatalf("version = %q, want %q", artifact.version, defaultVersion)
			}
			if artifact.url != want.url {
				t.Fatalf("url = %q, want %q", artifact.url, want.url)
			}
			if artifact.sha256 != want.sha256 {
				t.Fatalf("sha256 = %q, want %q", artifact.sha256, want.sha256)
			}
		})
	}
}

func TestRunStartsRuncAndReturnsProcess(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
run)
	write_args "$logdir/run-args" "$@"
	printf '%s' "$PWD" > "$logdir/run-pwd"
	cat > "$logdir/stdin"
	printf 'stdout from fake runc'
	printf 'stderr from fake runc' >&2
	touch "$logdir/run-started"
	while [ ! -f "$logdir/release" ]; do
		sleep 0.01
	done
	exit 0
	;;
*)
	exit 64
	;;
esac
`)
	bundlePath := privateTempDir(t)
	stateRoot := filepath.Join(privateTempDir(t), "state")
	runtime := runtimeWithBinary(t, binaryPath, stateRoot)
	process, err := runtime.Run(context.Background(), chamberRuntimeShared.RunRequest{
		Bundle: chamberBundleShared.ProvisionedBundle{
			ContainerID: "container-1",
			BundlePath:  bundlePath,
		},
		Stdin: strings.NewReader("stdin for fake runc"),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if process == nil {
		t.Fatal("Run() process = nil, want process")
	}
	waitForFile(t, filepath.Join(logDir, "run-started"))

	assertFileContent(t, filepath.Join(logDir, "run-pwd"), bundlePath)
	assertLines(t, filepath.Join(logDir, "run-args"), []string{"--root", stateRoot, "run", "container-1"})
	assertFileContent(t, filepath.Join(logDir, "stdin"), "stdin for fake runc")

	if err := os.WriteFile(filepath.Join(logDir, "release"), []byte("ok"), 0600); err != nil {
		t.Fatalf("WriteFile(release) error = %v", err)
	}
	exitCode, err := process.Wait()
	if err != nil {
		t.Fatalf("Process.Wait() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("Process.Wait() exit code = %d, want 0", exitCode)
	}
	stdoutContent, err := runtime.ReadLog("container-1", chamberRuntimeShared.StdoutLogStream)
	if err != nil {
		t.Fatalf("ReadLog(stdout) error = %v", err)
	}
	if string(stdoutContent) != "stdout from fake runc" {
		t.Fatalf("ReadLog(stdout) = %q, want fake runc stdout", string(stdoutContent))
	}
	stderrContent, err := runtime.ReadLog("container-1", chamberRuntimeShared.StderrLogStream)
	if err != nil {
		t.Fatalf("ReadLog(stderr) error = %v", err)
	}
	if string(stderrContent) != "stderr from fake runc" {
		t.Fatalf("ReadLog(stderr) = %q, want fake runc stderr", string(stderrContent))
	}
	stdoutPath := filepath.Join(stateRoot, "logs", "container-1", "stdout.log")
	if _, err := os.Stat(stdoutPath); err != nil {
		t.Fatalf("Stat(%q) error = %v", stdoutPath, err)
	}
}

func TestRunReturnsProcessForFastExit(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
run)
	exit 7
	;;
*)
	exit 64
	;;
esac
`)

	runtime := runtimeWithBinary(t, binaryPath, privateTempDir(t))
	process, err := runtime.Run(context.Background(), chamberRuntimeShared.RunRequest{
		Bundle: chamberBundleShared.ProvisionedBundle{
			ContainerID: "short.job",
			BundlePath:  privateTempDir(t),
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if process == nil {
		t.Fatal("Run() process = nil, want process")
	}

	for i := 0; i < 2; i++ {
		exitCode, err := process.Wait()
		if err != nil {
			t.Fatalf("Process.Wait() call %d error = %v", i+1, err)
		}
		if exitCode != 7 {
			t.Fatalf("Process.Wait() call %d exit code = %d, want 7", i+1, exitCode)
		}
	}

	if _, err := os.Stat(filepath.Join(logDir, "unused")); !os.IsNotExist(err) {
		t.Fatalf("unused marker stat error = %v, want not exist", err)
	}
}

func TestStateReadsRuncState(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
state)
	write_args "$logdir/state-args" "$@"
	printf '{"id":"stateful","status":"running"}'
	;;
*)
	exit 64
	;;
esac
`)

	stateRoot := privateTempDir(t)
	runtime := runtimeWithBinary(t, binaryPath, stateRoot)
	state, err := runtime.State(context.Background(), "stateful")
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if state.ContainerID != "stateful" || state.Status != chamberRuntimeShared.ContainerStatusRunning {
		t.Fatalf("State() = %#v, want stateful/running", state)
	}
	assertLines(t, filepath.Join(logDir, "state-args"), []string{"--root", stateRoot, "state", "stateful"})
}

func TestSignalSendsRuncKill(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
kill)
	write_args "$logdir/kill-args" "$@"
	;;
*)
	exit 64
	;;
esac
`)

	stateRoot := privateTempDir(t)
	runtime := runtimeWithBinary(t, binaryPath, stateRoot)
	err := runtime.Signal(context.Background(), chamberRuntimeShared.SignalRequest{
		ContainerID: "signaled",
		Signal:      chamberRuntimeShared.SignalTERM,
	})
	if err != nil {
		t.Fatalf("Signal() error = %v", err)
	}
	assertLines(t, filepath.Join(logDir, "kill-args"), []string{"--root", stateRoot, "kill", "signaled", "TERM"})
}

func TestSignalRejectsUnsupportedSignal(t *testing.T) {
	runtime := runtimeWithConfigOnly(t)

	err := runtime.Signal(context.Background(), chamberRuntimeShared.SignalRequest{
		ContainerID: "signaled",
		Signal:      chamberRuntimeShared.Signal("HUP"),
	})
	if err == nil {
		t.Fatal("Signal() error = nil, want unsupported signal error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("Signal() error = %v, want invalid request code", err)
	}
}

func TestDeleteRemovesRuncContainer(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
delete)
	write_args "$logdir/delete-args" "$@"
	;;
*)
	exit 64
	;;
esac
`)

	stateRoot := privateTempDir(t)
	runtime := runtimeWithBinary(t, binaryPath, stateRoot)
	err := runtime.Delete(context.Background(), chamberRuntimeShared.DeleteRequest{
		ContainerID: "deleted",
		Force:       true,
	})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	assertLines(t, filepath.Join(logDir, "delete-args"), []string{"--root", stateRoot, "delete", "--force", "deleted"})
}

func TestRunRejectsUnsafeContainerID(t *testing.T) {
	invalidContainerIDs := []string{
		"",
		"-starts-with-dash",
		".starts-with-dot",
		"has/slash",
		strings.Repeat("a", 129),
	}

	for _, containerID := range invalidContainerIDs {
		t.Run(containerID, func(t *testing.T) {
			runtime := runtimeWithConfigOnly(t)
			_, err := runtime.Run(context.Background(), chamberRuntimeShared.RunRequest{
				Bundle: chamberBundleShared.ProvisionedBundle{
					ContainerID: containerID,
					BundlePath:  privateTempDir(t),
				},
			})
			if err == nil {
				t.Fatal("Run() error = nil, want invalid container ID error")
			}
			if !strings.Contains(err.Error(), "invalid container ID") {
				t.Fatalf("Run() error = %v, want invalid container ID", err)
			}
		})
	}
}

func TestReadLogRejectsInvalidInput(t *testing.T) {
	runtime := runtimeWithConfigOnly(t)

	if _, err := runtime.ReadLog("container-logs", "stdin"); err == nil {
		t.Fatal("ReadLog(unsupported) error = nil, want error")
	}
	if _, err := runtime.ReadLog("", chamberRuntimeShared.StdoutLogStream); err == nil {
		t.Fatal("ReadLog(empty container ID) error = nil, want error")
	}
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:])
}

func withTestArtifact(content []byte) option {
	return withArtifact(runtimeArtifact{
		version: "test-version",
		url:     "https://example.invalid/runc",
		sha256:  sha256Hex(content),
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

func assertPrivateDir(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode = %o, want 0700", info.Mode().Perm())
	}
}

func runtimeWithBinary(t *testing.T, binaryPath string, stateRoot string) *Runtime {
	t.Helper()

	content, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", binaryPath, err)
	}
	return mustNew(t, chamberRuntimeShared.Config{
		RuntimeBinDir: filepath.Dir(binaryPath),
		RuntimeRoot:   stateRoot,
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withTestArtifact(content))
}

func runtimeWithConfigOnly(t *testing.T) *Runtime {
	t.Helper()

	content := []byte("binary")
	return mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(content))))), withTestArtifact(content))
}

func mustNew(t testing.TB, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager, options ...option) *Runtime {
	t.Helper()

	config = prepareRuntimeConfig(t, config, directoryManager)
	runtime, err := newWithOptions(context.Background(), config, directoryManager, options...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return runtime
}

func prepareRuntimeConfig(t testing.TB, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager) chamberRuntimeShared.Config {
	t.Helper()

	if config.Name == "" {
		config.Name = chamberRuntimeShared.RuntimeNameRunc
	}
	if config.Privilege == "" {
		config.Privilege = capability.Rootless
	}
	for _, path := range []string{config.RuntimeRoot, config.RuntimeBinDir} {
		if path == "" {
			continue
		}
		if err := directoryManager.MkdirPrivate(path); err != nil {
			t.Fatalf("MkdirPrivate(%q) error = %v", path, err)
		}
	}
	return config
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
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Body:       body,
	}
}

type interruptedBody struct {
	content []byte
	read    bool
}

func (body *interruptedBody) Read(destination []byte) (int, error) {
	if body.read {
		return 0, errors.New("interrupted")
	}
	body.read = true
	return copy(destination, body.content), nil
}

func (*interruptedBody) Close() error {
	return nil
}

func writeFakeRunc(t *testing.T, logDir string, body string) string {
	t.Helper()

	path := filepath.Join(privateTempDir(t), "runc")
	script := `#!/bin/sh
set -eu
logdir=` + shellQuote(logDir) + `
cmd="$3"

write_args() {
	path="$1"
	shift
	: > "$path"
	for arg in "$@"; do
		printf '%s\n' "$arg" >> "$path"
	done
}
` + body
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile(fake runc) error = %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func assertFileContent(t *testing.T, path string, want string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != want {
		t.Fatalf("ReadFile(%q) = %q, want %q", path, string(content), want)
	}
}

func assertLines(t *testing.T, path string, want []string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	got := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("lines in %q = %#v, want %#v", path, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d in %q = %q, want %q; all lines %#v", i+1, path, got[i], want[i], got)
		}
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertFileContentAndMode(t *testing.T, path string, wantContent []byte, wantMode os.FileMode) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != string(wantContent) {
		t.Fatalf("content at %q = %q, want %q", path, content, wantContent)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if info.Mode().Perm() != wantMode {
		t.Fatalf("mode at %q = %o, want %o", path, info.Mode().Perm(), wantMode)
	}
}
