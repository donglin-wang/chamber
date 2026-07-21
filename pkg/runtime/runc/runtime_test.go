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
	"syscall"
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

	descriptor := runtime.Descriptor()
	if descriptor.Name != "runc" {
		t.Fatalf("Descriptor().Name = %q, want runc", descriptor.Name)
	}
	if descriptor.BinaryPath != filepath.Join(binDir, "runc") {
		t.Fatalf("Descriptor().BinaryPath = %q, want %q", descriptor.BinaryPath, filepath.Join(binDir, "runc"))
	}
	if descriptor.Version != "test-version" {
		t.Fatalf("Descriptor().Version = %q, want test-version", descriptor.Version)
	}
	assertFileContentAndMode(t, descriptor.BinaryPath, content, 0755)
}

func TestNewDefaultsToRuncAdapterName(t *testing.T) {
	content := []byte("valid runc")
	binDir := privateTempDir(t)
	runtime := mustNew(t, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: binDir,
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, io.NopCloser(strings.NewReader(string(content))))), withTestArtifact(content))

	descriptor := runtime.Descriptor()
	if descriptor.Name != "runc" {
		t.Fatalf("Descriptor().Name = %q, want runc", descriptor.Name)
	}
	if descriptor.BinaryPath != filepath.Join(binDir, "runc") {
		t.Fatalf("Descriptor().BinaryPath = %q, want %q", descriptor.BinaryPath, filepath.Join(binDir, "runc"))
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
	if !errors.Is(err, chamberErrors.ErrRuntimeInstallFailed) {
		t.Fatalf("New() error = %v, want runtime install failed code", err)
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
	if !errors.Is(err, chamberErrors.ErrRuntimeInstallFailed) {
		t.Fatalf("New() error = %v, want runtime install failed code", err)
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
	if !errors.Is(err, chamberErrors.ErrRuntimeInstallFailed) {
		t.Fatalf("New() error = %v, want runtime install failed code", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "runc")); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestNewDownloadCancellationHasCanceledCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := newWithOptions(ctx, chamberRuntimeShared.Config{
		RuntimeRoot:   privateTempDir(t),
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
	}, localfs.NewDirectoryManager(), withHTTPClient(responseClient(http.StatusOK, &cancelingBody{cancel: cancel})), withTestArtifact([]byte("anything")))

	if err == nil {
		t.Fatal("New() error = nil, want cancellation error")
	}
	if !errors.Is(err, chamberErrors.ErrCanceled) {
		t.Fatalf("New() error = %v, want canceled code", err)
	}
	if errors.Is(err, chamberErrors.ErrRuntimeInstallFailed) {
		t.Fatalf("New() error = %v, should not include runtime install failed code", err)
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

	descriptor := runtime.Descriptor()
	if descriptor.BinaryPath != path {
		t.Fatalf("Descriptor().BinaryPath = %q, want %q", descriptor.BinaryPath, path)
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

	descriptor := runtime.Descriptor()
	if descriptor.BinaryPath != path {
		t.Fatalf("Descriptor().BinaryPath = %q, want %q", descriptor.BinaryPath, path)
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

	descriptor := runtime.Descriptor()
	if descriptor.BinaryPath != path {
		t.Fatalf("Descriptor().BinaryPath = %q, want %q", descriptor.BinaryPath, path)
	}
	assertFileContentAndMode(t, path, newContent, 0755)
}

func TestDescriptorDeclaresRuncSupport(t *testing.T) {
	runtime := &Runtime{
		binaryPath: "/tmp/runc",
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
	if descriptor.BinaryPath != "/tmp/runc" {
		t.Fatalf("Descriptor().BinaryPath = %q, want /tmp/runc", descriptor.BinaryPath)
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

func TestDescriptorNameDoesNotFollowBinaryPath(t *testing.T) {
	runtime := &Runtime{
		binaryPath: "/tmp/custom-runc-binary",
		artifact: runtimeArtifact{
			version: "test-version",
		},
	}

	descriptor := runtime.Descriptor()

	if descriptor.Name != "runc" {
		t.Fatalf("Descriptor().Name = %q, want runc adapter name", descriptor.Name)
	}
	if descriptor.BinaryPath != "/tmp/custom-runc-binary" {
		t.Fatalf("Descriptor().BinaryPath = %q, want configured binary path", descriptor.BinaryPath)
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

	descriptor := runtime.Descriptor()
	if !filepath.IsAbs(descriptor.BinaryPath) {
		t.Fatalf("Descriptor().BinaryPath = %q, want absolute path", descriptor.BinaryPath)
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

func TestDefaultRuntimeArtifactRejectsUnsupportedArchitectureWithHostCode(t *testing.T) {
	_, err := defaultRuntimeArtifact("mips")
	if err == nil {
		t.Fatal("defaultRuntimeArtifact() error = nil, want unsupported host error")
	}
	if !errors.Is(err, chamberErrors.ErrUnsupportedHost) {
		t.Fatalf("defaultRuntimeArtifact() error = %v, want unsupported host code", err)
	}
}

func TestRunStartsRuncAndReturnsContainer(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
run)
	write_args "$logdir/run-args" "$@"
	printf '%s' "$PWD" > "$logdir/run-pwd"
	touch "$logdir/run-started"
	cat > "$logdir/stdin"
	touch "$logdir/stdin-read"
	printf 'stdout from fake runc'
	printf 'stderr from fake runc' >&2
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
	var streamedStdout strings.Builder
	var streamedStderr strings.Builder
	container, err := runtime.Run(context.Background(), chamberRuntimeShared.RunRequest{
		Bundle: chamberBundleShared.ProvisionedBundle{
			ContainerID: "container-1",
			BundlePath:  bundlePath,
		},
		Stdin:  strings.NewReader("stdin for fake runc"),
		Stdout: []io.Writer{&streamedStdout},
		Stderr: []io.Writer{&streamedStderr},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if container == nil {
		t.Fatal("Run() container = nil, want container")
	}
	if container.ID() != "container-1" {
		t.Fatalf("Container.ID() = %q, want container-1", container.ID())
	}
	waitForFile(t, filepath.Join(logDir, "run-started"))

	assertFileContent(t, filepath.Join(logDir, "run-pwd"), bundlePath)
	assertLines(t, filepath.Join(logDir, "run-args"), []string{"--root", stateRoot, "run", "container-1"})
	waitForFile(t, filepath.Join(logDir, "stdin-read"))
	assertFileContent(t, filepath.Join(logDir, "stdin"), "stdin for fake runc")

	if err := os.WriteFile(filepath.Join(logDir, "release"), []byte("ok"), 0600); err != nil {
		t.Fatalf("WriteFile(release) error = %v", err)
	}
	result, err := container.Wait()
	if err != nil {
		t.Fatalf("Container.Wait() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("Container.Wait() exit code = %d, want 0", result.ExitCode)
	}
	stdoutContent, err := container.ReadLog(chamberRuntimeShared.StdoutLogStream)
	if err != nil {
		t.Fatalf("ReadLog(stdout) error = %v", err)
	}
	if string(stdoutContent) != "stdout from fake runc" {
		t.Fatalf("ReadLog(stdout) = %q, want fake runc stdout", string(stdoutContent))
	}
	stderrContent, err := container.ReadLog(chamberRuntimeShared.StderrLogStream)
	if err != nil {
		t.Fatalf("ReadLog(stderr) error = %v", err)
	}
	if string(stderrContent) != "stderr from fake runc" {
		t.Fatalf("ReadLog(stderr) = %q, want fake runc stderr", string(stderrContent))
	}
	stdoutPath := filepath.Join(stateRoot, "logs", "container-1", "stdout.log")
	stderrPath := filepath.Join(stateRoot, "logs", "container-1", "stderr.log")
	if container.StdoutPath() != stdoutPath {
		t.Fatalf("Container.StdoutPath() = %q, want %q", container.StdoutPath(), stdoutPath)
	}
	if container.StderrPath() != stderrPath {
		t.Fatalf("Container.StderrPath() = %q, want %q", container.StderrPath(), stderrPath)
	}
	if streamedStdout.String() != "stdout from fake runc" {
		t.Fatalf("streamed stdout = %q, want fake runc stdout", streamedStdout.String())
	}
	if streamedStderr.String() != "stderr from fake runc" {
		t.Fatalf("streamed stderr = %q, want fake runc stderr", streamedStderr.String())
	}
}

func TestRunReturnsContainerForFastExit(t *testing.T) {
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
	container, err := runtime.Run(context.Background(), chamberRuntimeShared.RunRequest{
		Bundle: chamberBundleShared.ProvisionedBundle{
			ContainerID: "short.job",
			BundlePath:  privateTempDir(t),
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if container == nil {
		t.Fatal("Run() container = nil, want container")
	}

	for i := 0; i < 2; i++ {
		result, err := container.Wait()
		if err != nil {
			t.Fatalf("Container.Wait() call %d error = %v", i+1, err)
		}
		if result.ExitCode != 7 {
			t.Fatalf("Container.Wait() call %d exit code = %d, want 7", i+1, result.ExitCode)
		}
	}

	if _, err := os.Stat(filepath.Join(logDir, "unused")); !os.IsNotExist(err) {
		t.Fatalf("unused marker stat error = %v, want not exist", err)
	}
}

func TestRunStartFailureHasErrorCodeAndRemovesLogs(t *testing.T) {
	stateRoot := privateTempDir(t)
	runtime := &Runtime{
		config: chamberRuntimeShared.Config{
			RuntimeRoot: stateRoot,
		},
		binaryPath:       filepath.Join(t.TempDir(), "missing-runc"),
		directoryManager: localfs.NewDirectoryManager(),
	}

	_, err := runtime.Run(context.Background(), chamberRuntimeShared.RunRequest{
		Bundle: chamberBundleShared.ProvisionedBundle{
			ContainerID: "start-fails",
			BundlePath:  privateTempDir(t),
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want start failure")
	}
	if !errors.Is(err, chamberErrors.ErrRuntimeStartFailed) {
		t.Fatalf("Run() error = %v, want runtime start failed code", err)
	}
	for _, path := range []string{
		filepath.Join(stateRoot, "logs", "start-fails", "stdout.log"),
		filepath.Join(stateRoot, "logs", "start-fails", "stderr.log"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("log path %q stat error = %v, want not exist", path, statErr)
		}
	}
}

func TestRunContextCancellationDoesNotStopContainer(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
run)
	touch "$logdir/run-started"
	while true; do
		if [ -f "$logdir/release" ]; then
			exit 0
		fi
		sleep 0.01
	done
	;;
kill)
	write_args "$logdir/kill-args" "$@"
	touch "$logdir/release"
	;;
*)
	exit 64
	;;
esac
`)
	ctx, cancel := context.WithCancel(context.Background())
	stateRoot := privateTempDir(t)
	runtime := runtimeWithBinary(t, binaryPath, stateRoot)
	container, err := runtime.Run(ctx, chamberRuntimeShared.RunRequest{
		Bundle: chamberBundleShared.ProvisionedBundle{
			ContainerID: "cancelled",
			BundlePath:  privateTempDir(t),
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	waitForFile(t, filepath.Join(logDir, "run-started"))

	cancel()
	select {
	case <-container.(*runcContainer).done:
		t.Fatal("container exited after Run context cancellation; lifecycle should remain container-owned")
	case <-time.After(100 * time.Millisecond):
	}

	if err := container.Signal(context.Background(), syscall.SIGTERM); err != nil {
		t.Fatalf("Signal() error = %v", err)
	}
	assertLines(t, filepath.Join(logDir, "kill-args"), []string{"--root", stateRoot, "kill", "cancelled", "15"})
	result, err := container.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("Wait() exit code = %d, want 0", result.ExitCode)
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
	container := &runcContainer{containerConfig: containerConfig{
		id:         "stateful",
		binaryPath: binaryPath,
		stateRoot:  stateRoot,
	}}
	state, err := container.State(context.Background())
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
	container := &runcContainer{containerConfig: containerConfig{
		id:         "signaled",
		binaryPath: binaryPath,
		stateRoot:  stateRoot,
	}}
	err := container.Signal(context.Background(), syscall.SIGTERM)
	if err != nil {
		t.Fatalf("Signal() error = %v", err)
	}
	assertLines(t, filepath.Join(logDir, "kill-args"), []string{"--root", stateRoot, "kill", "signaled", "15"})
}

func TestSignalFailureHasRuntimeControlCode(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
kill)
	printf 'cannot signal' >&2
	exit 33
	;;
*)
	exit 64
	;;
esac
`)

	container := &runcContainer{containerConfig: containerConfig{
		id:         "signaled",
		binaryPath: binaryPath,
		stateRoot:  privateTempDir(t),
	}}
	err := container.Signal(context.Background(), syscall.SIGTERM)
	if err == nil {
		t.Fatal("Signal() error = nil, want control failure")
	}
	if !errors.Is(err, chamberErrors.ErrRuntimeControlFailed) {
		t.Fatalf("Signal() error = %v, want runtime control failed code", err)
	}
}

func TestSignalRejectsUnsupportedSignal(t *testing.T) {
	container := &runcContainer{containerConfig: containerConfig{
		id:         "signaled",
		binaryPath: "/tmp/runc",
		stateRoot:  privateTempDir(t),
	}}

	err := container.Signal(context.Background(), unsupportedSignal("unsupported"))
	if err == nil {
		t.Fatal("Signal() error = nil, want unsupported signal error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("Signal() error = %v, want invalid request code", err)
	}
}

type unsupportedSignal string

func (signal unsupportedSignal) String() string {
	return string(signal)
}

func (unsupportedSignal) Signal() {}

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
	container := &runcContainer{containerConfig: containerConfig{
		id:         "deleted",
		binaryPath: binaryPath,
		stateRoot:  stateRoot,
	}}
	err := container.Delete(context.Background(), true)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	assertLines(t, filepath.Join(logDir, "delete-args"), []string{"--root", stateRoot, "delete", "--force", "deleted"})
}

func TestDeleteFailureHasRuntimeControlCode(t *testing.T) {
	logDir := privateTempDir(t)
	binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
delete)
	printf 'cannot delete' >&2
	exit 44
	;;
*)
	exit 64
	;;
esac
`)

	container := &runcContainer{containerConfig: containerConfig{
		id:         "deleted",
		binaryPath: binaryPath,
		stateRoot:  privateTempDir(t),
	}}
	err := container.Delete(context.Background(), true)
	if err == nil {
		t.Fatal("Delete() error = nil, want control failure")
	}
	if !errors.Is(err, chamberErrors.ErrRuntimeControlFailed) {
		t.Fatalf("Delete() error = %v, want runtime control failed code", err)
	}
}

func TestControlCommandCancellationHasCanceledCode(t *testing.T) {
	tests := map[string]func(context.Context, chamberRuntimeShared.Container) error{
		"state": func(ctx context.Context, container chamberRuntimeShared.Container) error {
			_, err := container.State(ctx)
			return err
		},
		"signal": func(ctx context.Context, container chamberRuntimeShared.Container) error {
			return container.Signal(ctx, syscall.SIGTERM)
		},
		"delete": func(ctx context.Context, container chamberRuntimeShared.Container) error {
			return container.Delete(ctx, true)
		},
	}

	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			logDir := privateTempDir(t)
			binaryPath := writeFakeRunc(t, logDir, `
case "$cmd" in
state|kill|delete)
	touch "$logdir/$cmd-started"
	while true; do
		sleep 1
	done
	;;
*)
	exit 64
	;;
esac
`)
			container := &runcContainer{containerConfig: containerConfig{
				id:         "cancelled-control",
				binaryPath: binaryPath,
				stateRoot:  privateTempDir(t),
			}}
			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() {
				errCh <- call(ctx, container)
			}()
			marker := name
			if marker == "signal" {
				marker = "kill"
			}
			waitForFile(t, filepath.Join(logDir, marker+"-started"))

			cancel()
			select {
			case err := <-errCh:
				if err == nil {
					t.Fatal("control command error = nil, want cancellation error")
				}
				if !errors.Is(err, chamberErrors.ErrCanceled) {
					t.Fatalf("control command error = %v, want canceled code", err)
				}
				if errors.Is(err, chamberErrors.ErrRuntimeControlFailed) {
					t.Fatalf("control command error = %v, should not include runtime control failed code", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("control command did not return after context cancellation")
			}
		})
	}
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

func TestContainerLogMethodsRejectInvalidStream(t *testing.T) {
	container := &runcContainer{containerConfig: containerConfig{
		id:         "container-logs",
		binaryPath: "/tmp/runc",
		stateRoot:  privateTempDir(t),
		stdoutPath: filepath.Join(privateTempDir(t), "stdout.log"),
		stderrPath: filepath.Join(privateTempDir(t), "stderr.log"),
	}}

	if _, err := container.ReadLog("stdin"); err == nil {
		t.Fatal("ReadLog(unsupported) error = nil, want error")
	}
	if err := container.DeleteLog("stdin"); err == nil {
		t.Fatal("DeleteLog(unsupported) error = nil, want error")
	}
}

func TestReadLogMissingFileHasLogNotFoundCode(t *testing.T) {
	container := &runcContainer{containerConfig: containerConfig{
		id:         "container-logs",
		binaryPath: "/tmp/runc",
		stateRoot:  privateTempDir(t),
		stdoutPath: filepath.Join(privateTempDir(t), "stdout.log"),
		stderrPath: filepath.Join(privateTempDir(t), "stderr.log"),
	}}

	_, err := container.ReadLog(chamberRuntimeShared.StdoutLogStream)
	if err == nil {
		t.Fatal("ReadLog(stdout) error = nil, want missing log error")
	}
	if !errors.Is(err, chamberErrors.ErrLogNotFound) {
		t.Fatalf("ReadLog(stdout) error = %v, want log not found code", err)
	}
}

func TestDeleteLogRemovesSelectedStream(t *testing.T) {
	logDir := privateTempDir(t)
	stdoutPath := filepath.Join(logDir, "stdout.log")
	stderrPath := filepath.Join(logDir, "stderr.log")
	if err := os.WriteFile(stdoutPath, []byte("stdout"), 0600); err != nil {
		t.Fatalf("WriteFile(stdout) error = %v", err)
	}
	if err := os.WriteFile(stderrPath, []byte("stderr"), 0600); err != nil {
		t.Fatalf("WriteFile(stderr) error = %v", err)
	}
	container := &runcContainer{containerConfig: containerConfig{
		id:         "container-logs",
		binaryPath: "/tmp/runc",
		stateRoot:  privateTempDir(t),
		stdoutPath: stdoutPath,
		stderrPath: stderrPath,
	}}

	if err := container.DeleteLog(chamberRuntimeShared.StdoutLogStream); err != nil {
		t.Fatalf("DeleteLog(stdout) error = %v", err)
	}
	if _, err := os.Stat(stdoutPath); !os.IsNotExist(err) {
		t.Fatalf("stdout log stat error = %v, want not exist", err)
	}
	assertFileContent(t, stderrPath, "stderr")
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

type cancelingBody struct {
	cancel context.CancelFunc
}

func (body *cancelingBody) Read([]byte) (int, error) {
	body.cancel()
	return 0, context.Canceled
}

func (*cancelingBody) Close() error {
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

	deadline := time.Now().Add(10 * time.Second)
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
