package runc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"

	chruntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

const (
	DefaultVersion = "v1.5.0"

	defaultAMD64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.amd64"
	defaultAMD64SHA256 = "0363e69bebd3a027d1239364ab9b4f4873f6bc4e7a7878e94b4ea59f08551297"
	defaultARM64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.arm64"
	defaultARM64SHA256 = "1f6d8c553add066a6aaf838d3172d4c5ed3c6b065b6f7eed2f4a4aa4af261e59"
)

var validContainerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

var (
	startupObservationTimeout = 500 * time.Millisecond
	startupPollInterval       = 10 * time.Millisecond
)

var _ chruntime.Runtime = (*Runtime)(nil)

type Runtime struct {
	config           chruntime.Config
	client           *http.Client
	directoryManager localfs.DirectoryManager
}

type Option func(*Runtime)

func WithHTTPClient(client *http.Client) Option {
	return func(runtime *Runtime) {
		if client != nil {
			runtime.client = client
		}
	}
}

func New(config chruntime.Config, directoryManager localfs.DirectoryManager, options ...Option) *Runtime {
	if config.Name == "" {
		config.Name = chruntime.DefaultName
	}
	if config.Version == "" {
		config.Version = DefaultVersion
	}
	if config.URL == "" && config.SHA256 == "" {
		config.URL, config.SHA256 = defaultRuntimeArtifact(goruntime.GOARCH)
	}
	runtime := &Runtime{
		config:           config,
		client:           http.DefaultClient,
		directoryManager: directoryManager,
	}
	for _, option := range options {
		option(runtime)
	}

	return runtime
}

func (r *Runtime) Ensure(ctx context.Context) (chruntime.Binary, error) {
	config := r.config
	if r.directoryManager == nil {
		return chruntime.Binary{}, fmt.Errorf("directory manager is required")
	}
	if config.Version == "" || config.URL == "" || config.SHA256 == "" {
		return chruntime.Binary{}, fmt.Errorf("runc runtime requires version, url, and sha256")
	}
	expectedDigest, err := parseSHA256(config.SHA256)
	if err != nil {
		return chruntime.Binary{}, err
	}

	binary, err := r.configuredBinary()
	if err != nil {
		return chruntime.Binary{}, err
	}
	binDir := filepath.Dir(binary.Path)
	if err := r.directoryManager.EnsurePrivateDir(binDir); err != nil {
		return chruntime.Binary{}, fmt.Errorf("prepare runtime bin dir: %w", err)
	}

	if ok, err := fileMatchesSHA256(binary.Path, expectedDigest); err != nil {
		return chruntime.Binary{}, fmt.Errorf("verify existing runtime binary: %w", err)
	} else if ok {
		return binary, nil
	}

	if err := r.download(ctx, config.URL, expectedDigest, binDir, binary.Path); err != nil {
		return chruntime.Binary{}, err
	}

	return binary, nil
}

func (r *Runtime) Run(ctx context.Context, request chruntime.RunRequest) (chruntime.StartResult, error) {
	if request.Bundle.BundlePath == "" {
		return chruntime.StartResult{}, fmt.Errorf("runtime bundle path is required")
	}
	containerID := request.Bundle.ContainerID
	if !validContainerID.MatchString(containerID) {
		return chruntime.StartResult{}, fmt.Errorf("invalid container ID %q", containerID)
	}
	if len(request.Bundle.RootFS.Mounts) > 0 {
		return chruntime.StartResult{}, fmt.Errorf("runtime bundle rootfs mounts are not yet supported by runc Run")
	}
	binary, err := r.configuredBinary()
	if err != nil {
		return chruntime.StartResult{}, err
	}
	stateRoot, err := r.stateRoot()
	if err != nil {
		return chruntime.StartResult{}, err
	}
	directoryManager, err := r.requireDirectoryManager()
	if err != nil {
		return chruntime.StartResult{}, err
	}
	if err := directoryManager.EnsurePrivateDir(stateRoot); err != nil {
		return chruntime.StartResult{}, fmt.Errorf("prepare runtime state root: %w", err)
	}
	stdout, stderr, err := r.openLogs(containerID)
	if err != nil {
		return chruntime.StartResult{}, err
	}

	cmd := exec.CommandContext(ctx, binary.Path, "--root", stateRoot, "run", containerID)
	cmd.Dir = request.Bundle.BundlePath
	cmd.Stdin = request.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return chruntime.StartResult{}, fmt.Errorf("start runc container %q: %w", containerID, err)
	}

	process := newRuncProcess(cmd, stdout, stderr)
	state, err := observeStartup(ctx, binary.Path, stateRoot, containerID, process)
	if err != nil {
		return chruntime.StartResult{}, err
	}
	return chruntime.StartResult{
		Process: process,
		State:   state,
	}, nil
}

func (r *Runtime) ReadLog(containerID string, stream string) ([]byte, error) {
	path, err := r.logPath(containerID, stream)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (r *Runtime) openLogs(containerID string) (*os.File, *os.File, error) {
	logDir, err := r.logDir(containerID)
	if err != nil {
		return nil, nil, err
	}
	directoryManager, err := r.requireDirectoryManager()
	if err != nil {
		return nil, nil, err
	}
	if err := directoryManager.EnsurePrivateDir(logDir); err != nil {
		return nil, nil, fmt.Errorf("prepare runc log directory: %w", err)
	}

	stdoutPath, err := r.logPath(containerID, chruntime.StdoutLogStream)
	if err != nil {
		return nil, nil, err
	}
	stderrPath, err := r.logPath(containerID, chruntime.StderrLogStream)
	if err != nil {
		return nil, nil, err
	}

	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open stdout log: %w", err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("open stderr log: %w", err)
	}
	return stdout, stderr, nil
}

func (r *Runtime) logPath(containerID string, stream string) (string, error) {
	logDir, err := r.logDir(containerID)
	if err != nil {
		return "", err
	}
	switch stream {
	case chruntime.StdoutLogStream, chruntime.StderrLogStream:
		return filepath.Join(logDir, stream+".log"), nil
	default:
		return "", fmt.Errorf("unsupported log stream %q", stream)
	}
}

func (r *Runtime) logDir(containerID string) (string, error) {
	if !validContainerID.MatchString(containerID) {
		return "", fmt.Errorf("invalid container ID %q", containerID)
	}
	runtimeRoot, err := r.stateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeRoot, "logs", containerID), nil
}

func (r *Runtime) stateRoot() (string, error) {
	if r.config.RuntimeRoot == "" {
		return "", fmt.Errorf("runtime root is required")
	}
	runtimeRoot, err := absPath(r.config.RuntimeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve runtime root: %w", err)
	}
	return runtimeRoot, nil
}

func (r *Runtime) configuredBinary() (chruntime.Binary, error) {
	config := r.config
	if config.Name == "" {
		return chruntime.Binary{}, fmt.Errorf("runtime name is required")
	}
	if config.RuntimeBinDir == "" {
		return chruntime.Binary{}, fmt.Errorf("runtime bin dir is required")
	}
	binDir, err := absPath(config.RuntimeBinDir)
	if err != nil {
		return chruntime.Binary{}, fmt.Errorf("resolve runtime bin dir: %w", err)
	}
	return chruntime.Binary{
		Name:    config.Name,
		Version: config.Version,
		Path:    filepath.Join(binDir, config.Name),
	}, nil
}

func defaultRuntimeArtifact(arch string) (url string, sha256 string) {
	switch arch {
	case "amd64":
		return defaultAMD64URL, defaultAMD64SHA256
	case "arm64":
		return defaultARM64URL, defaultARM64SHA256
	default:
		return "", ""
	}
}

func (r *Runtime) requireDirectoryManager() (localfs.DirectoryManager, error) {
	if r.directoryManager == nil {
		return nil, fmt.Errorf("directory manager is required")
	}
	return r.directoryManager, nil
}

type runcProcess struct {
	done   chan struct{}
	result waitResult
}

type waitResult struct {
	exitCode int
	err      error
}

func newRuncProcess(cmd *exec.Cmd, closers ...io.Closer) *runcProcess {
	process := &runcProcess{
		done: make(chan struct{}),
	}
	go func() {
		process.result = convertWaitResult(cmd.Wait())
		for _, closer := range closers {
			if closeErr := closer.Close(); closeErr != nil && process.result.err == nil {
				process.result.err = fmt.Errorf("close runtime stdio: %w", closeErr)
			}
		}
		close(process.done)
	}()
	return process
}

func (p *runcProcess) Wait() (int, error) {
	<-p.done
	return p.result.exitCode, p.result.err
}

func convertWaitResult(err error) waitResult {
	if err == nil {
		return waitResult{exitCode: 0}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode := exitErr.ExitCode()
		if exitCode >= 0 {
			return waitResult{exitCode: exitCode}
		}
		return waitResult{
			exitCode: exitCode,
			err:      fmt.Errorf("runtime process exited without an exit code: %w", err),
		}
	}

	return waitResult{err: err}
}

func observeStartup(ctx context.Context, binaryPath string, stateRoot string, containerID string, process *runcProcess) (chruntime.ObservedState, error) {
	ticker := time.NewTicker(startupPollInterval)
	defer ticker.Stop()

	deadline := time.NewTimer(startupObservationTimeout)
	defer deadline.Stop()

	for {
		if running, err := containerIsRunning(ctx, binaryPath, stateRoot, containerID); err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("observe runc container %q startup: %w", containerID, err)
			}
		} else if running {
			return chruntime.ProcessRunning, nil
		}

		select {
		case <-process.done:
			return chruntime.ProcessExited, nil
		case <-deadline.C:
			return chruntime.ProcessRunning, nil
		case <-ctx.Done():
			return "", fmt.Errorf("observe runc container %q startup: %w", containerID, ctx.Err())
		case <-ticker.C:
		}
	}
}

type runcState struct {
	Status string `json:"status"`
}

func containerIsRunning(ctx context.Context, binaryPath string, stateRoot string, containerID string) (bool, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "--root", stateRoot, "state", containerID)
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}

	var state runcState
	if err := json.Unmarshal(output, &state); err != nil {
		return false, err
	}
	return state.Status == "running", nil
}

func (r *Runtime) download(ctx context.Context, url string, expectedDigest []byte, binDir string, binaryPath string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create runtime download request: %w", err)
	}

	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("download runtime binary: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download runtime binary: unexpected HTTP status %s", response.Status)
	}

	tmp, err := r.directoryManager.CreateTemp(binDir, "."+filepath.Base(binaryPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary runtime binary: %w", err)
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
		return fmt.Errorf("download runtime binary: %w", err)
	}
	actualDigest := digest.Sum(nil)
	if !equalDigest(actualDigest, expectedDigest) {
		_ = tmp.Close()
		return fmt.Errorf("verify runtime binary checksum: got %s, want %s", hex.EncodeToString(actualDigest), hex.EncodeToString(expectedDigest))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync runtime binary: %w", err)
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set runtime binary mode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close runtime binary: %w", err)
	}
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		return fmt.Errorf("commit runtime binary: %w", err)
	}
	committed = true

	return nil
}

func fileMatchesSHA256(path string, expectedDigest []byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
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

func parseSHA256(raw string) ([]byte, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "sha256:")
	digest, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("parse runtime sha256: %w", err)
	}
	if len(digest) != sha256.Size {
		return nil, fmt.Errorf("parse runtime sha256: got %d bytes, want %d", len(digest), sha256.Size)
	}
	return digest, nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
