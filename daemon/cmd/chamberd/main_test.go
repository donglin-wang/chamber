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
		"--http-addr", "127.0.0.1:8080",
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
	if options.httpAddr != "127.0.0.1:8080" {
		t.Fatalf("httpAddr = %q, want 127.0.0.1:8080", options.httpAddr)
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
	if options.httpAddr != "" {
		t.Fatalf("httpAddr = %q, want empty", options.httpAddr)
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

func TestRootlessPreflightAcceptsEnabledPrerequisites(t *testing.T) {
	readFile := mapReadFile(map[string]string{
		"/proc/sys/kernel/unprivileged_userns_clone":             "1\n",
		"/proc/sys/user/max_user_namespaces":                     "28633\n",
		"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "0\n",
	})
	if err := rootlessPreflight(readFile); err != nil {
		t.Fatalf("rootlessPreflight returned error: %v", err)
	}
}

func TestRootlessPreflightIgnoresMissingPrerequisiteFiles(t *testing.T) {
	if err := rootlessPreflight(mapReadFile(nil)); err != nil {
		t.Fatalf("rootlessPreflight returned error: %v", err)
	}
}

func TestRootlessPreflightRejectsDisabledUserNamespaces(t *testing.T) {
	readFile := mapReadFile(map[string]string{
		"/proc/sys/kernel/unprivileged_userns_clone": "0\n",
		"/proc/sys/user/max_user_namespaces":         "28633\n",
	})
	err := rootlessPreflight(readFile)
	if err == nil || !strings.Contains(err.Error(), "unprivileged user namespaces are disabled") {
		t.Fatalf("rootlessPreflight error = %v, want disabled userns error", err)
	}
}

func TestRootlessPreflightRejectsZeroUserNamespaceLimit(t *testing.T) {
	readFile := mapReadFile(map[string]string{
		"/proc/sys/kernel/unprivileged_userns_clone": "1\n",
		"/proc/sys/user/max_user_namespaces":         "0\n",
	})
	err := rootlessPreflight(readFile)
	if err == nil || !strings.Contains(err.Error(), "user namespaces are unavailable") {
		t.Fatalf("rootlessPreflight error = %v, want namespace limit error", err)
	}
}

func TestRootlessPreflightRejectsAppArmorRestriction(t *testing.T) {
	readFile := mapReadFile(map[string]string{
		"/proc/sys/kernel/unprivileged_userns_clone":             "1\n",
		"/proc/sys/user/max_user_namespaces":                     "28633\n",
		"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "1\n",
	})
	err := rootlessPreflight(readFile)
	if err == nil || !strings.Contains(err.Error(), "restricted by AppArmor") {
		t.Fatalf("rootlessPreflight error = %v, want AppArmor restriction error", err)
	}
}

func TestPrepareDaemonPathsCreatesDaemonOwnedDirectories(t *testing.T) {
	root := t.TempDir()
	socketPath := filepath.Join(root, "run", "chamber.sock")
	tmpRoot := filepath.Join(root, "run", "tmp")

	if err := prepareDaemonPaths(socketPath, tmpRoot, localDirectoryManager{}); err != nil {
		t.Fatalf("prepareDaemonPaths returned error: %v", err)
	}

	for _, path := range []string{filepath.Dir(socketPath), tmpRoot} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%q is not a directory", path)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("%q mode = %o, want 0700", path, info.Mode().Perm())
		}
	}
}

func TestPrepareDaemonPathsRejectsUnsafeTmpRoot(t *testing.T) {
	root := t.TempDir()
	socketPath := filepath.Join(root, "run", "chamber.sock")
	tmpRoot := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmpRoot, 0755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Chmod(tmpRoot, 0755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	err := prepareDaemonPaths(socketPath, tmpRoot, localDirectoryManager{})
	if err == nil {
		t.Fatal("prepareDaemonPaths returned nil error")
	}
	if !strings.Contains(err.Error(), "prepare tmp root") {
		t.Fatalf("prepareDaemonPaths error = %v, want tmp root context", err)
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

func TestListenDaemonAPIUsesUnixSocketByDefault(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "chamber.sock")

	endpoints, err := listenDaemonAPI(path, "", os.Geteuid())
	if err != nil {
		t.Fatalf("listenDaemonAPI returned error: %v", err)
	}
	defer cleanupAPIEndpoints(endpoints)

	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(endpoints))
	}
	if endpoints[0].name != "unix" {
		t.Fatalf("endpoint name = %q, want unix", endpoints[0].name)
	}
	if got := endpoints[0].listener.Addr().Network(); got != "unix" {
		t.Fatalf("listener network = %q, want unix", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestListenDaemonAPIAddsTCPDemoListener(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "chamber.sock")

	endpoints, err := listenDaemonAPI(path, "127.0.0.1:0", os.Geteuid())
	if err != nil {
		t.Fatalf("listenDaemonAPI returned error: %v", err)
	}
	defer cleanupAPIEndpoints(endpoints)

	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(endpoints))
	}
	if endpoints[0].name != "unix" || endpoints[1].name != "tcp" {
		t.Fatalf("endpoint names = %q, %q; want unix, tcp", endpoints[0].name, endpoints[1].name)
	}
	if got := endpoints[1].listener.Addr().Network(); got != "tcp" {
		t.Fatalf("TCP listener network = %q, want tcp", got)
	}
	if got := endpoints[1].listener.Addr().String(); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("TCP listener address = %q, want 127.0.0.1:<port>", got)
	}
}

func TestListenDaemonAPIRejectsInvalidTCPAddressAndRemovesSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "chamber.sock")

	err := mustFailListenDaemonAPI(path, "127.0.0.1:not-a-port")
	if !strings.Contains(err.Error(), "listen on TCP address") {
		t.Fatalf("listenDaemonAPI error = %v, want TCP context", err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("socket still exists after TCP listen failure, err=%v", statErr)
	}
}

func mustFailListenDaemonAPI(socketPath string, httpAddr string) error {
	endpoints, err := listenDaemonAPI(socketPath, httpAddr, os.Geteuid())
	if err == nil {
		cleanupAPIEndpoints(endpoints)
		return errors.New("listenDaemonAPI returned nil error")
	}
	return err
}

type localDirectoryManager struct{}

func (localDirectoryManager) EnsurePrivateDir(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return errors.New("not a directory")
		}
		if info.Mode().Perm()&0077 != 0 {
			return errors.New("must not be readable, writable, or executable by group or other users")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.MkdirAll(path, 0700)
}

func (m localDirectoryManager) EnsurePrivateParent(path string) error {
	return m.EnsurePrivateDir(filepath.Dir(path))
}

func (localDirectoryManager) MkdirTemp(parent string, pattern string) (string, error) {
	return os.MkdirTemp(parent, pattern)
}

func (localDirectoryManager) CreateTemp(parent string, pattern string) (*os.File, error) {
	return os.CreateTemp(parent, pattern)
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

func mapReadFile(files map[string]string) readFileFunc {
	return func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(value), nil
	}
}
