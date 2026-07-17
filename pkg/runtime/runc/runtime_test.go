package runc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func TestEnsureDownloadsValidRuntimeBinary(t *testing.T) {
	content := []byte("valid runc")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	binDir := privateTempDir(t)
	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	if binary.Name != "runc" {
		t.Fatalf("Binary.Name = %q, want runc", binary.Name)
	}
	if binary.Version != "test-version" {
		t.Fatalf("Binary.Version = %q, want test-version", binary.Version)
	}
	if binary.Path != filepath.Join(binDir, "runc") {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, filepath.Join(binDir, "runc"))
	}
	assertFileContentAndMode(t, binary.Path, content, 0755)
}

func TestEnsureRejectsWrongDigest(t *testing.T) {
	content := []byte("not the pinned binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	binDir := privateTempDir(t)
	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex([]byte("expected binary")),
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want digest error")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Ensure() error = %v, want checksum failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "runc")); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestEnsureRejectsNonOKResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex([]byte("anything")),
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want HTTP status error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("Ensure() error = %v, want HTTP 404", err)
	}
}

func TestEnsureRejectsInterruptedBody(t *testing.T) {
	content := []byte("partial")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(content)+10, content)
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)

	binDir := privateTempDir(t)
	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want interrupted body error")
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "runc")); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestEnsureUsesExistingValidBinary(t *testing.T) {
	content := []byte("already cached")
	binDir := privateTempDir(t)
	path := filepath.Join(binDir, "runc")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	t.Cleanup(server.Close)

	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if binary.Path != path {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, path)
	}
	if requests != 0 {
		t.Fatalf("download requests = %d, want 0", requests)
	}
	assertFileContentAndMode(t, path, content, 0755)
}

func TestEnsureReplacesExistingInvalidBinary(t *testing.T) {
	oldContent := []byte("corrupt cached binary")
	newContent := []byte("replacement binary")

	binDir := privateTempDir(t)
	path := filepath.Join(binDir, "runc")
	if err := os.WriteFile(path, oldContent, 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(newContent)
	}))
	t.Cleanup(server.Close)

	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(newContent),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if binary.Path != path {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, path)
	}
	assertFileContentAndMode(t, path, newContent, 0755)
}

func TestEnsureReturnsAbsolutePath(t *testing.T) {
	content := []byte("absolute")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	relativeBinDir := filepath.Join(".", t.Name())
	t.Cleanup(func() {
		_ = os.RemoveAll(relativeBinDir)
	})

	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: relativeBinDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if !filepath.IsAbs(binary.Path) {
		t.Fatalf("Binary.Path = %q, want absolute path", binary.Path)
	}
}

func TestEnsureRequiresCompleteConfiguration(t *testing.T) {
	runtime := New(chamberRuntime.Config{
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
		Version:       "test-version",
		URL:           "http://example.test/runc",
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want configuration error")
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
			url, sha256 := defaultRuntimeArtifact(arch)
			if url != want.url {
				t.Fatalf("url = %q, want %q", url, want.url)
			}
			if sha256 != want.sha256 {
				t.Fatalf("sha256 = %q, want %q", sha256, want.sha256)
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
	process, err := runtime.Run(context.Background(), chamberRuntime.RunRequest{
		Bundle: chamberBundle.ProvisionedBundle{
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
	stdoutContent, err := runtime.ReadLog("container-1", chamberRuntime.StdoutLogStream)
	if err != nil {
		t.Fatalf("ReadLog(stdout) error = %v", err)
	}
	if string(stdoutContent) != "stdout from fake runc" {
		t.Fatalf("ReadLog(stdout) = %q, want fake runc stdout", string(stdoutContent))
	}
	stderrContent, err := runtime.ReadLog("container-1", chamberRuntime.StderrLogStream)
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
	process, err := runtime.Run(context.Background(), chamberRuntime.RunRequest{
		Bundle: chamberBundle.ProvisionedBundle{
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
	if state.ContainerID != "stateful" || state.Status != "running" {
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
	err := runtime.Signal(context.Background(), chamberRuntime.SignalRequest{
		ContainerID: "signaled",
		Signal:      "TERM",
	})
	if err != nil {
		t.Fatalf("Signal() error = %v", err)
	}
	assertLines(t, filepath.Join(logDir, "kill-args"), []string{"--root", stateRoot, "kill", "signaled", "TERM"})
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
	err := runtime.Delete(context.Background(), chamberRuntime.DeleteRequest{
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
			runtime := New(chamberRuntime.Config{}, nil)
			_, err := runtime.Run(context.Background(), chamberRuntime.RunRequest{
				Bundle: chamberBundle.ProvisionedBundle{
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

func TestRunRejectsRootFSMountsForNow(t *testing.T) {
	runtime := New(chamberRuntime.Config{}, nil)
	_, err := runtime.Run(context.Background(), chamberRuntime.RunRequest{
		Bundle: chamberBundle.ProvisionedBundle{
			ContainerID: "with-mounts",
			BundlePath:  privateTempDir(t),
			RootFS: chamberBundle.RootFS{
				Mounts: []chamberBundle.Mount{{
					Type:   "bind",
					Source: "/tmp/source",
					Target: "target",
				}},
			},
		},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want unsupported mounts error")
	}
	if !strings.Contains(err.Error(), "mounts are not yet supported") {
		t.Fatalf("Run() error = %v, want unsupported mounts message", err)
	}
}

func TestReadLogRejectsInvalidInput(t *testing.T) {
	runtime := New(chamberRuntime.Config{
		RuntimeRoot: privateTempDir(t),
	}, localfs.NewDirectoryManager())

	if _, err := runtime.ReadLog("container-logs", "stdin"); err == nil {
		t.Fatal("ReadLog(unsupported) error = nil, want error")
	}
	if _, err := runtime.ReadLog("", chamberRuntime.StdoutLogStream); err == nil {
		t.Fatal("ReadLog(empty container ID) error = nil, want error")
	}
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:])
}

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func runtimeWithBinary(t *testing.T, binaryPath string, stateRoot string) *Runtime {
	t.Helper()

	content, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", binaryPath, err)
	}
	return New(chamberRuntime.Config{
		RuntimeBinDir: filepath.Dir(binaryPath),
		RuntimeRoot:   stateRoot,
		Name:          filepath.Base(binaryPath),
		Version:       "test-version",
		URL:           "https://example.invalid/runc",
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())
}

func writeFakeRunc(t *testing.T, logDir string, body string) string {
	t.Helper()

	path := filepath.Join(privateTempDir(t), "fake-runc")
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
