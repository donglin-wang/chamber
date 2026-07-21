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
	"strconv"
	"strings"
	"syscall"

	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	"github.com/donglin-wang/chamber/pkg/shared/containerid"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

const (
	runtimeName    = chamberRuntimeShared.RuntimeNameRunc
	defaultVersion = "v1.5.0"

	defaultAMD64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.amd64"
	defaultAMD64SHA256 = "0363e69bebd3a027d1239364ab9b4f4873f6bc4e7a7878e94b4ea59f08551297"
	defaultARM64URL    = "https://github.com/opencontainers/runc/releases/download/v1.5.0/runc.arm64"
	defaultARM64SHA256 = "1f6d8c553add066a6aaf838d3172d4c5ed3c6b065b6f7eed2f4a4aa4af261e59"
)

var _ chamberRuntimeShared.Runtime = (*Runtime)(nil)
var _ chamberRuntimeShared.Container = (*runcContainer)(nil)

var capabilities = chamberRuntimeShared.Capabilities{
	Privileges: []capability.Privilege{
		capability.Rootless,
	},
	Isolation: []chamberRuntimeShared.Isolation{
		chamberRuntimeShared.ProcessIsolation,
	},
}

type Runtime struct {
	config           chamberRuntimeShared.Config
	binaryPath       string
	binary           runtimeBinary
	client           *http.Client
	directoryManager localfs.DirectoryManager
	logger           *chamberLogging.SlogLogger
}

type option func(*Runtime)

type runtimeBinary struct {
	version string
	url     string
	sha256  string
}

func New(ctx context.Context, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager) (*Runtime, error) {
	return newWithOptions(ctx, config, directoryManager)
}

func newWithOptions(ctx context.Context, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager, options ...option) (*Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: runtime construction canceled before start: %w", chamberErrors.ErrCanceled, err)
	}

	logger, err := chamberLogging.LoggerFromConfig(config.Logging, nil)
	if err != nil {
		return nil, err
	}

	binary, err := defaultRuntimeBinary(goruntime.GOARCH)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{
		config:           config,
		binary:           binary,
		client:           http.DefaultClient,
		directoryManager: directoryManager,
		logger:           logger,
	}
	for _, option := range options {
		option(runtime)
	}
	binaryPath, err := configuredBinaryPath(config)
	if err != nil {
		return nil, err
	}
	runtime.binaryPath = binaryPath
	if err := runtime.installBinary(ctx); err != nil {
		return nil, err
	}

	return runtime, nil
}

func (r *Runtime) Descriptor() chamberRuntimeShared.Descriptor {
	version := defaultVersion
	binaryPath := ""
	if r != nil {
		if r.binary.version != "" {
			version = r.binary.version
		}
		binaryPath = r.binaryPath
	}
	return chamberRuntimeShared.Descriptor{
		Name:         runtimeName,
		Version:      version,
		BinaryPath:   binaryPath,
		Capabilities: chamberRuntimeShared.CloneCapabilities(capabilities),
	}
}

func (r *Runtime) installBinary(ctx context.Context) error {
	if r == nil || r.directoryManager == nil {
		return fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	binary := r.binary
	if binary.version == "" || binary.url == "" || binary.sha256 == "" {
		return fmt.Errorf("%w: runc runtime requires version, url, and sha256", chamberErrors.ErrInvalidRequest)
	}
	expectedDigest, err := parseSHA256(binary.sha256)
	if err != nil {
		return err
	}

	binaryPath := r.binaryPath
	binDir := filepath.Dir(binaryPath)

	if ok, err := fileMatchesSHA256(binaryPath, expectedDigest); err != nil {
		return fmt.Errorf("%w: verify existing runtime binary: %w", chamberErrors.ErrRuntimeInstallFailed, err)
	} else if ok {
		if err := os.Chmod(binaryPath, 0755); err != nil {
			return fmt.Errorf("%w: make existing runtime binary executable: %w", chamberErrors.ErrRuntimeInstallFailed, err)
		}
		chamberLogging.InfoWith(r.logger, ctx, "runtime binary ready",
			"runtime", runtimeName,
			"version", binary.version,
			"path", binaryPath,
			"source", "cache",
		)
		return nil
	}

	chamberLogging.InfoWith(r.logger, ctx, "downloading runtime binary",
		"runtime", runtimeName,
		"version", binary.version,
		"url", binary.url,
		"path", binaryPath,
	)
	if err := r.download(ctx, binary.url, expectedDigest, binDir, binaryPath); err != nil {
		return err
	}

	chamberLogging.InfoWith(r.logger, ctx, "runtime binary ready",
		"runtime", runtimeName,
		"version", binary.version,
		"path", binaryPath,
		"source", "download",
	)
	return nil
}

func (r *Runtime) Run(ctx context.Context, request chamberRuntimeShared.RunRequest) (chamberRuntimeShared.Container, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: runtime launch canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if request.Bundle.BundlePath == "" {
		return nil, fmt.Errorf("%w: runtime bundle path is required", chamberErrors.ErrInvalidRequest)
	}
	containerID := request.Bundle.ContainerID
	if err := containerid.Validate(containerID); err != nil {
		return nil, err
	}
	binaryPath := r.binaryPath
	if binaryPath == "" {
		return nil, fmt.Errorf("%w: runtime binary is required", chamberErrors.ErrInvalidRequest)
	}
	stateRoot, err := r.stateRoot()
	if err != nil {
		return nil, err
	}
	chamberLogging.InfoWith(r.logger, ctx, "starting runtime container",
		"container_id", containerID,
		"bundle_path", request.Bundle.BundlePath,
		"state_root", stateRoot,
	)
	stdout, stderr, err := r.openLogs(containerID)
	if err != nil {
		return nil, err
	}
	stdoutPath, err := r.logPath(containerID, chamberRuntimeShared.StdoutLogStream)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}
	stderrPath, err := r.logPath(containerID, chamberRuntimeShared.StderrLogStream)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}

	cmd := exec.Command(binaryPath, "--root", stateRoot, "run", containerID)
	cmd.Dir = request.Bundle.BundlePath
	cmd.Stdin = request.Stdin
	cmd.Stdout = outputWriter(stdout, request.Stdout)
	cmd.Stderr = outputWriter(stderr, request.Stderr)

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		_ = os.Remove(stdoutPath)
		_ = os.Remove(stderrPath)
		return nil, fmt.Errorf("%w: start runc container %q: %w", chamberErrors.ErrRuntimeStartFailed, containerID, err)
	}

	chamberLogging.InfoWith(r.logger, ctx, "started runtime container",
		"container_id", containerID,
		"pid", cmd.Process.Pid,
	)
	return newRuncContainer(containerConfig{
		id:         containerID,
		binaryPath: binaryPath,
		stateRoot:  stateRoot,
		stdoutPath: stdoutPath,
		stderrPath: stderrPath,
		logger:     r.logger,
	}, cmd, stdout, stderr), nil
}

func (r *Runtime) openLogs(containerID string) (*os.File, *os.File, error) {
	logDir, err := r.logDir(containerID)
	if err != nil {
		return nil, nil, err
	}
	if r.directoryManager == nil {
		return nil, nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if err := r.directoryManager.MkdirPrivate(logDir); err != nil {
		return nil, nil, fmt.Errorf("%w: create runc log directory: %v", chamberErrors.ErrFilesystemFailed, err)
	}

	stdoutPath, err := r.logPath(containerID, chamberRuntimeShared.StdoutLogStream)
	if err != nil {
		return nil, nil, err
	}
	stderrPath, err := r.logPath(containerID, chamberRuntimeShared.StderrLogStream)
	if err != nil {
		return nil, nil, err
	}

	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: open stdout log: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("%w: open stderr log: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	return stdout, stderr, nil
}

func outputWriter(logFile io.Writer, writers []io.Writer) io.Writer {
	all := make([]io.Writer, 0, len(writers)+1)
	all = append(all, logFile)
	for _, writer := range writers {
		if writer != nil {
			all = append(all, writer)
		}
	}
	return io.MultiWriter(all...)
}

func (r *Runtime) logPath(containerID string, stream chamberRuntimeShared.LogStream) (string, error) {
	logDir, err := r.logDir(containerID)
	if err != nil {
		return "", err
	}
	switch stream {
	case chamberRuntimeShared.StdoutLogStream, chamberRuntimeShared.StderrLogStream:
		return filepath.Join(logDir, string(stream)+".log"), nil
	default:
		return "", fmt.Errorf("%w: unsupported log stream %q", chamberErrors.ErrInvalidRequest, stream)
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

func (r *Runtime) stateRoot() (string, error) {
	if r.config.RuntimeRoot == "" {
		return "", fmt.Errorf("%w: runtime root is required", chamberErrors.ErrInvalidRequest)
	}
	runtimeRoot, err := absPath(r.config.RuntimeRoot)
	if err != nil {
		return "", fmt.Errorf("%w: resolve runtime root: %w", chamberErrors.ErrInvalidRequest, err)
	}
	return runtimeRoot, nil
}

func configuredBinaryPath(config chamberRuntimeShared.Config) (string, error) {
	if config.RuntimeBinDir == "" {
		return "", fmt.Errorf("%w: runtime bin dir is required", chamberErrors.ErrInvalidRequest)
	}
	binDir, err := absPath(config.RuntimeBinDir)
	if err != nil {
		return "", fmt.Errorf("%w: resolve runtime bin dir: %w", chamberErrors.ErrInvalidRequest, err)
	}
	return filepath.Join(binDir, runtimeName), nil
}

func defaultRuntimeBinary(arch string) (runtimeBinary, error) {
	switch arch {
	case "amd64":
		return runtimeBinary{version: defaultVersion, url: defaultAMD64URL, sha256: defaultAMD64SHA256}, nil
	case "arm64":
		return runtimeBinary{version: defaultVersion, url: defaultARM64URL, sha256: defaultARM64SHA256}, nil
	default:
		return runtimeBinary{}, fmt.Errorf("%w: runc runtime does not have a default binary for architecture %q", chamberErrors.ErrUnsupportedHost, arch)
	}
}

type containerConfig struct {
	id         string
	binaryPath string
	stateRoot  string
	stdoutPath string
	stderrPath string
	logger     *chamberLogging.SlogLogger
}

type runcContainer struct {
	containerConfig
	done   chan struct{}
	result waitResult
}

type waitResult struct {
	exitCode int
	err      error
}

func newRuncContainer(config containerConfig, cmd *exec.Cmd, closers ...io.Closer) *runcContainer {
	container := &runcContainer{
		containerConfig: config,
		done:            make(chan struct{}),
	}
	go func() {
		container.result = convertWaitResult(cmd.Wait())
		for _, closer := range closers {
			if closeErr := closer.Close(); closeErr != nil && container.result.err == nil {
				container.result.err = fmt.Errorf("%w: close runtime stdio: %w", chamberErrors.ErrRuntimeWaitFailed, closeErr)
			}
		}
		close(container.done)
	}()
	return container
}

func (c *runcContainer) ID() string {
	if c == nil {
		return ""
	}
	return c.id
}

func (c *runcContainer) StdoutPath() string {
	if c == nil {
		return ""
	}
	return c.stdoutPath
}

func (c *runcContainer) StderrPath() string {
	if c == nil {
		return ""
	}
	return c.stderrPath
}

func (c *runcContainer) Wait() (chamberRuntimeShared.ContainerResult, error) {
	<-c.done
	return chamberRuntimeShared.ContainerResult{
		ExitCode: c.result.exitCode,
	}, c.result.err
}

func (c *runcContainer) State(ctx context.Context) (chamberRuntimeShared.ContainerState, error) {
	if ctx == nil {
		return chamberRuntimeShared.ContainerState{}, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return chamberRuntimeShared.ContainerState{}, fmt.Errorf("%w: runtime state canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if c == nil {
		return chamberRuntimeShared.ContainerState{}, fmt.Errorf("%w: runtime container is required", chamberErrors.ErrInvalidRequest)
	}
	state, err := readRuncState(ctx, c.binaryPath, c.stateRoot, c.id)
	if err != nil {
		if errors.Is(err, chamberErrors.ErrCanceled) {
			return chamberRuntimeShared.ContainerState{}, err
		}
		return chamberRuntimeShared.ContainerState{}, fmt.Errorf("%w: read runc state: %w", chamberErrors.ErrRuntimeControlFailed, err)
	}
	return chamberRuntimeShared.ContainerState{
		ContainerID: c.id,
		Status:      chamberRuntimeShared.ContainerStatus(state.Status),
	}, nil
}

func (c *runcContainer) Signal(ctx context.Context, signal os.Signal) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: runtime signal canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if c == nil {
		return fmt.Errorf("%w: runtime container is required", chamberErrors.ErrInvalidRequest)
	}
	signalArg, err := signalArgument(signal)
	if err != nil {
		return err
	}
	chamberLogging.InfoWith(c.logger, ctx, "signaling runtime container",
		"container_id", c.id,
		"signal", signal,
	)
	cmd := exec.CommandContext(ctx, c.binaryPath, "--root", c.stateRoot, "kill", c.id, signalArg)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: runtime signal canceled while running control command: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: signal runc container %q: %w: %s", chamberErrors.ErrRuntimeControlFailed, c.id, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func signalArgument(signal os.Signal) (string, error) {
	if signal == nil {
		return "", fmt.Errorf("%w: runtime signal is required", chamberErrors.ErrInvalidRequest)
	}
	syscallSignal, ok := signal.(syscall.Signal)
	if !ok {
		return "", fmt.Errorf("%w: unsupported runtime signal %q", chamberErrors.ErrInvalidRequest, signal)
	}
	if syscallSignal <= 0 {
		return "", fmt.Errorf("%w: unsupported runtime signal %q", chamberErrors.ErrInvalidRequest, signal)
	}
	return strconv.Itoa(int(syscallSignal)), nil
}

func (c *runcContainer) Delete(ctx context.Context, force bool) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: runtime delete canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if c == nil {
		return fmt.Errorf("%w: runtime container is required", chamberErrors.ErrInvalidRequest)
	}
	args := []string{"--root", c.stateRoot, "delete"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, c.id)
	chamberLogging.InfoWith(c.logger, ctx, "deleting runtime container",
		"container_id", c.id,
		"force", force,
	)
	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: runtime delete canceled while running control command: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: delete runc container %q: %w: %s", chamberErrors.ErrRuntimeControlFailed, c.id, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *runcContainer) ReadLog(stream chamberRuntimeShared.LogStream) ([]byte, error) {
	path, err := c.logPath(stream)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: read runtime log %q: %w", chamberErrors.ErrLogNotFound, path, err)
		}
		return nil, fmt.Errorf("%w: read runtime log %q: %w", chamberErrors.ErrRuntimeControlFailed, path, err)
	}
	return content, nil
}

func (c *runcContainer) DeleteLog(stream chamberRuntimeShared.LogStream) error {
	path, err := c.logPath(stream)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: delete runtime log %q: %w", chamberErrors.ErrRuntimeControlFailed, path, err)
	}
	return nil
}

func (c *runcContainer) logPath(stream chamberRuntimeShared.LogStream) (string, error) {
	if c == nil {
		return "", fmt.Errorf("%w: runtime container is required", chamberErrors.ErrInvalidRequest)
	}
	switch stream {
	case chamberRuntimeShared.StdoutLogStream:
		return c.stdoutPath, nil
	case chamberRuntimeShared.StderrLogStream:
		return c.stderrPath, nil
	default:
		return "", fmt.Errorf("%w: unsupported log stream %q", chamberErrors.ErrInvalidRequest, stream)
	}
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
			err:      fmt.Errorf("%w: runtime process exited without an exit code: %w", chamberErrors.ErrRuntimeWaitFailed, err),
		}
	}

	return waitResult{err: fmt.Errorf("%w: wait for runtime process: %w", chamberErrors.ErrRuntimeWaitFailed, err)}
}

type runcState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func readRuncState(ctx context.Context, binaryPath string, stateRoot string, containerID string) (runcState, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "--root", stateRoot, "state", containerID)
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return runcState{}, fmt.Errorf("%w: runtime state canceled while running control command: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return runcState{}, err
	}
	var state runcState
	if err := json.Unmarshal(output, &state); err != nil {
		return runcState{}, err
	}
	return state, nil
}

func (r *Runtime) download(ctx context.Context, url string, expectedDigest []byte, binDir string, binaryPath string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: create runtime download request: %w", chamberErrors.ErrRuntimeInstallFailed, err)
	}

	response, err := r.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: runtime binary download canceled while requesting: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: download runtime binary: %w", chamberErrors.ErrRuntimeInstallFailed, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: download runtime binary: unexpected HTTP status %s", chamberErrors.ErrRuntimeInstallFailed, response.Status)
	}

	tmp, err := r.directoryManager.CreateTemp(binDir, "."+filepath.Base(binaryPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary runtime binary: %v", chamberErrors.ErrFilesystemFailed, err)
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
			return fmt.Errorf("%w: runtime binary download canceled while reading response: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: download runtime binary: %w", chamberErrors.ErrRuntimeInstallFailed, err)
	}
	actualDigest := digest.Sum(nil)
	if !equalDigest(actualDigest, expectedDigest) {
		_ = tmp.Close()
		return fmt.Errorf("%w: verify runtime binary checksum: got %s, want %s", chamberErrors.ErrRuntimeInstallFailed, hex.EncodeToString(actualDigest), hex.EncodeToString(expectedDigest))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: sync runtime binary: %w", chamberErrors.ErrRuntimeInstallFailed, err)
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: set runtime binary mode: %w", chamberErrors.ErrRuntimeInstallFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close runtime binary: %w", chamberErrors.ErrRuntimeInstallFailed, err)
	}
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		return fmt.Errorf("%w: commit runtime binary: %w", chamberErrors.ErrRuntimeInstallFailed, err)
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
		return nil, fmt.Errorf("%w: parse runtime sha256: %w", chamberErrors.ErrInvalidRequest, err)
	}
	if len(digest) != sha256.Size {
		return nil, fmt.Errorf("%w: parse runtime sha256: got %d bytes, want %d", chamberErrors.ErrInvalidRequest, len(digest), sha256.Size)
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
