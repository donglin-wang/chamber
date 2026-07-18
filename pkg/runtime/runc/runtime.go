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
	goruntime "runtime"
	"strings"

	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/containerid"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

const (
	DefaultVersion = "v1.5.0"

	defaultAMD64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.amd64"
	defaultAMD64SHA256 = "0363e69bebd3a027d1239364ab9b4f4873f6bc4e7a7878e94b4ea59f08551297"
	defaultARM64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.arm64"
	defaultARM64SHA256 = "1f6d8c553add066a6aaf838d3172d4c5ed3c6b065b6f7eed2f4a4aa4af261e59"
)

var _ chamberRuntime.Runtime = (*Runtime)(nil)

type Runtime struct {
	config           chamberRuntime.Config
	binary           chamberRuntime.Binary
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

func New(ctx context.Context, config chamberRuntime.Config, directoryManager localfs.DirectoryManager, options ...Option) (*Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if directoryManager == nil {
		return nil, fmt.Errorf("directory manager is required")
	}
	if err := chamberLogging.Configure(config.Logging, nil); err != nil {
		return nil, err
	}
	if config.Name == "" {
		config.Name = chamberRuntime.DefaultName
	}
	if config.Version == "" {
		config.Version = DefaultVersion
	}
	if config.URL == "" && config.SHA256 == "" {
		config.URL, config.SHA256 = defaultRuntimeArtifact(goruntime.GOARCH)
	}
	resolved, err := chamberRuntime.Resolve(config, chamberRuntime.Override{})
	if err != nil {
		return nil, err
	}

	binary, err := configuredBinary(resolved)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{
		config:           resolved,
		binary:           binary,
		client:           http.DefaultClient,
		directoryManager: directoryManager,
	}
	for _, option := range options {
		option(runtime)
	}

	stateRoot, err := runtime.stateRoot()
	if err != nil {
		return nil, err
	}
	if err := directoryManager.MkdirPrivate(stateRoot); err != nil {
		return nil, fmt.Errorf("create runtime state root: %w", err)
	}
	binDir := filepath.Dir(binary.Path)
	if err := directoryManager.MkdirPrivate(binDir); err != nil {
		return nil, fmt.Errorf("create runtime bin dir: %w", err)
	}
	if err := runtime.installBinary(ctx); err != nil {
		return nil, err
	}

	return runtime, nil
}

func (r *Runtime) Binary() chamberRuntime.Binary {
	if r == nil {
		return chamberRuntime.Binary{}
	}
	return r.binary
}

func (r *Runtime) installBinary(ctx context.Context) error {
	if r == nil || r.directoryManager == nil {
		return fmt.Errorf("directory manager is required")
	}
	config := r.config
	if config.Version == "" || config.URL == "" || config.SHA256 == "" {
		return fmt.Errorf("runc runtime requires version, url, and sha256")
	}
	expectedDigest, err := parseSHA256(config.SHA256)
	if err != nil {
		return err
	}

	binary := r.binary
	binDir := filepath.Dir(binary.Path)

	if ok, err := fileMatchesSHA256(binary.Path, expectedDigest); err != nil {
		return fmt.Errorf("verify existing runtime binary: %w", err)
	} else if ok {
		chamberLogging.Info(ctx, "runtime binary ready",
			"runtime", binary.Name,
			"version", binary.Version,
			"path", binary.Path,
			"source", "cache",
		)
		return nil
	}

	chamberLogging.Info(ctx, "downloading runtime binary",
		"runtime", binary.Name,
		"version", binary.Version,
		"url", config.URL,
		"path", binary.Path,
	)
	if err := r.download(ctx, config.URL, expectedDigest, binDir, binary.Path); err != nil {
		return err
	}

	chamberLogging.Info(ctx, "runtime binary ready",
		"runtime", binary.Name,
		"version", binary.Version,
		"path", binary.Path,
		"source", "download",
	)
	return nil
}

func (r *Runtime) Run(ctx context.Context, request chamberRuntime.RunRequest) (chamberRuntime.Process, error) {
	if request.Bundle.BundlePath == "" {
		return nil, fmt.Errorf("runtime bundle path is required")
	}
	containerID := request.Bundle.ContainerID
	if err := containerid.Validate(containerID); err != nil {
		return nil, err
	}
	if len(request.Bundle.RootFS.Mounts) > 0 {
		return nil, fmt.Errorf("runtime bundle rootfs mounts are not yet supported by runc Run")
	}
	binary := r.binary
	if binary.Path == "" {
		return nil, fmt.Errorf("runtime binary is required")
	}
	stateRoot, err := r.stateRoot()
	if err != nil {
		return nil, err
	}
	chamberLogging.Info(ctx, "starting runtime container",
		"container_id", containerID,
		"bundle_path", request.Bundle.BundlePath,
		"state_root", stateRoot,
	)
	stdout, stderr, err := r.openLogs(containerID)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, binary.Path, "--root", stateRoot, "run", containerID)
	cmd.Dir = request.Bundle.BundlePath
	cmd.Stdin = request.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start runc container %q: %w", containerID, err)
	}

	chamberLogging.Info(ctx, "started runtime container",
		"container_id", containerID,
		"pid", cmd.Process.Pid,
	)
	return newRuncProcess(cmd, stdout, stderr), nil
}

func (r *Runtime) ReadLog(containerID string, stream string) ([]byte, error) {
	path, err := r.logPath(containerID, stream)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (r *Runtime) State(ctx context.Context, containerID string) (chamberRuntime.ContainerState, error) {
	if err := containerid.Validate(containerID); err != nil {
		return chamberRuntime.ContainerState{}, err
	}
	binary, stateRoot, err := r.binaryAndStateRoot()
	if err != nil {
		return chamberRuntime.ContainerState{}, err
	}
	state, err := readRuncState(ctx, binary.Path, stateRoot, containerID)
	if err != nil {
		return chamberRuntime.ContainerState{}, err
	}
	return chamberRuntime.ContainerState{
		ContainerID: containerID,
		Status:      state.Status,
	}, nil
}

func (r *Runtime) Signal(ctx context.Context, request chamberRuntime.SignalRequest) error {
	if err := containerid.Validate(request.ContainerID); err != nil {
		return err
	}
	if strings.TrimSpace(request.Signal) == "" {
		return fmt.Errorf("%w: runtime signal is required", chamberErrors.ErrInvalidRequest)
	}
	binary, stateRoot, err := r.binaryAndStateRoot()
	if err != nil {
		return err
	}
	chamberLogging.Info(ctx, "signaling runtime container",
		"container_id", request.ContainerID,
		"signal", request.Signal,
	)
	cmd := exec.CommandContext(ctx, binary.Path, "--root", stateRoot, "kill", request.ContainerID, request.Signal)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("signal runc container %q: %w: %s", request.ContainerID, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r *Runtime) Delete(ctx context.Context, request chamberRuntime.DeleteRequest) error {
	if err := containerid.Validate(request.ContainerID); err != nil {
		return err
	}
	binary, stateRoot, err := r.binaryAndStateRoot()
	if err != nil {
		return err
	}
	args := []string{"--root", stateRoot, "delete"}
	if request.Force {
		args = append(args, "--force")
	}
	args = append(args, request.ContainerID)
	chamberLogging.Info(ctx, "deleting runtime container",
		"container_id", request.ContainerID,
		"force", request.Force,
	)
	cmd := exec.CommandContext(ctx, binary.Path, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete runc container %q: %w: %s", request.ContainerID, err, strings.TrimSpace(string(output)))
	}
	return nil
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
	if err := directoryManager.MkdirPrivate(logDir); err != nil {
		return nil, nil, fmt.Errorf("create runc log directory: %w", err)
	}

	stdoutPath, err := r.logPath(containerID, chamberRuntime.StdoutLogStream)
	if err != nil {
		return nil, nil, err
	}
	stderrPath, err := r.logPath(containerID, chamberRuntime.StderrLogStream)
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
	case chamberRuntime.StdoutLogStream, chamberRuntime.StderrLogStream:
		return filepath.Join(logDir, stream+".log"), nil
	default:
		return "", fmt.Errorf("unsupported log stream %q", stream)
	}
}

func (r *Runtime) logDir(containerID string) (string, error) {
	if err := containerid.Validate(containerID); err != nil {
		return "", err
	}
	runtimeRoot, err := r.stateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeRoot, "logs", containerID), nil
}

func (r *Runtime) binaryAndStateRoot() (chamberRuntime.Binary, string, error) {
	binary := r.binary
	if binary.Path == "" {
		return chamberRuntime.Binary{}, "", fmt.Errorf("runtime binary is required")
	}
	stateRoot, err := r.stateRoot()
	if err != nil {
		return chamberRuntime.Binary{}, "", err
	}
	return binary, stateRoot, nil
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

func configuredBinary(config chamberRuntime.Config) (chamberRuntime.Binary, error) {
	if config.Name == "" {
		return chamberRuntime.Binary{}, fmt.Errorf("runtime name is required")
	}
	if config.RuntimeBinDir == "" {
		return chamberRuntime.Binary{}, fmt.Errorf("runtime bin dir is required")
	}
	binDir, err := absPath(config.RuntimeBinDir)
	if err != nil {
		return chamberRuntime.Binary{}, fmt.Errorf("resolve runtime bin dir: %w", err)
	}
	return chamberRuntime.Binary{
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

type runcState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func readRuncState(ctx context.Context, binaryPath string, stateRoot string, containerID string) (runcState, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "--root", stateRoot, "state", containerID)
	output, err := cmd.Output()
	if err != nil {
		return runcState{}, err
	}
	return decodeRuncState(output)
}

func decodeRuncState(output []byte) (runcState, error) {
	var state runcState
	if err := json.Unmarshal(output, &state); err != nil {
		return runcState{}, err
	}
	return state, nil
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
