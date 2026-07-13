package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseArgsBuildsConfigOverride(t *testing.T) {
	options, err := parseArgs([]string{
		"--socket-path", "run/chamber.sock",
		"--tmp-root", "tmp",
		"--container-root", "containers",
		"--image-root", "images",
		"--runtime-root", "runtime",
		"--runtime-bin-dir", "bin",
		"--runtime-name", "runc",
		"--runtime-version", "v1.5.0",
		"--runtime-url", "https://example.test/runc",
		"--runtime-sha256", "abc123",
		"--metadata-root", "metadata",
		"--log-level", "debug",
		"--log-format", "text",
		"--platform", "linux/amd64",
	})
	if err != nil {
		t.Fatalf("parseArgs returned error: %v", err)
	}

	assertStringPtr(t, "SocketPath", options.override.SocketPath, "run/chamber.sock")
	assertStringPtr(t, "TmpRoot", options.override.TmpRoot, "tmp")
	assertStringPtr(t, "ContainerRoot", options.override.ContainerRoot, "containers")
	assertStringPtr(t, "Image.Root", options.override.Image.Root, "images")
	assertStringPtr(t, "Runtime.RuntimeRoot", options.override.Runtime.RuntimeRoot, "runtime")
	assertStringPtr(t, "Runtime.RuntimeBinDir", options.override.Runtime.RuntimeBinDir, "bin")
	assertStringPtr(t, "Runtime.Name", options.override.Runtime.Name, "runc")
	assertStringPtr(t, "Runtime.Version", options.override.Runtime.Version, "v1.5.0")
	assertStringPtr(t, "Runtime.URL", options.override.Runtime.URL, "https://example.test/runc")
	assertStringPtr(t, "Runtime.SHA256", options.override.Runtime.SHA256, "abc123")
	assertStringPtr(t, "Metadata.Root", options.override.Metadata.Root, "metadata")
	assertStringPtr(t, "LogLevel", options.override.LogLevel, "debug")
	assertStringPtr(t, "LogFormat", options.override.LogFormat, "text")
	if options.platform != "linux/amd64" {
		t.Fatalf("platform = %q, want linux/amd64", options.platform)
	}
}

func TestParseArgsLeavesUnsetOverridesNil(t *testing.T) {
	options, err := parseArgs(nil)
	if err != nil {
		t.Fatalf("parseArgs returned error: %v", err)
	}
	if options.override.SocketPath != nil {
		t.Fatalf("SocketPath override = %q, want nil", *options.override.SocketPath)
	}
	if options.override.Image.Root != nil {
		t.Fatalf("Image.Root override = %q, want nil", *options.override.Image.Root)
	}
	if options.override.Runtime.RuntimeRoot != nil {
		t.Fatalf("Runtime.RuntimeRoot override = %q, want nil", *options.override.Runtime.RuntimeRoot)
	}
	if options.override.Metadata.Root != nil {
		t.Fatalf("Metadata.Root override = %q, want nil", *options.override.Metadata.Root)
	}
	if options.platform != "" {
		t.Fatalf("platform = %q, want empty", options.platform)
	}
}

func TestParseArgsRejectsPositionalArguments(t *testing.T) {
	_, err := parseArgs([]string{"serve"})
	if err == nil {
		t.Fatal("parseArgs returned nil error, want positional argument error")
	}
	if !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("parseArgs error = %v, want positional argument error", err)
	}
}

func TestRuntimePreflight(t *testing.T) {
	tests := []struct {
		name string
		goos string
		euid int
		want string
	}{
		{
			name: "accepts linux non-root",
			goos: "linux",
			euid: 501,
		},
		{
			name: "rejects non-linux",
			goos: "darwin",
			euid: 501,
			want: "requires linux",
		},
		{
			name: "rejects root",
			goos: "linux",
			euid: 0,
			want: "must not run as root",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := runtimePreflight(test.goos, test.euid)
			if test.want == "" {
				if err != nil {
					t.Fatalf("runtimePreflight returned error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runtimePreflight error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestCryptoHexIDGeneratorProducesRuntimeSafeIDs(t *testing.T) {
	generator := cryptoHexIDGenerator{}
	validRuntimeID := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,127}$`)
	seen := make(map[string]struct{})

	for i := 0; i < 1000; i++ {
		id := generator.New()
		if !validRuntimeID.MatchString(id) {
			t.Fatalf("generated id %q is not runtime-safe", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("generated duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestPrepareUnixSocketRemovesOwnedStaleSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "chamber.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	if err := prepareUnixSocket(path, os.Geteuid()); err != nil {
		t.Fatalf("prepareUnixSocket returned error: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file still exists after prepareUnixSocket, err=%v", err)
	}
}

func TestPrepareUnixSocketRejectsActiveSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "chamber.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()

	err = prepareUnixSocket(path, os.Geteuid())
	if err == nil || !strings.Contains(err.Error(), "listening server") {
		t.Fatalf("prepareUnixSocket error = %v, want listening server error", err)
	}

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("active socket dial did not reach listener")
	}
}

func TestPrepareUnixSocketRejectsNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chamber.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	err := prepareUnixSocket(path, os.Geteuid())
	if err == nil || !strings.Contains(err.Error(), "not a unix socket") {
		t.Fatalf("prepareUnixSocket error = %v, want non-socket error", err)
	}
}

func assertStringPtr(t *testing.T, name string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %q", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %q, want %q", name, *got, want)
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "chd-")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}
