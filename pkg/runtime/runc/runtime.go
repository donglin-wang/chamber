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
	binary           chamberRuntimeShared.Binary
	artifact         runtimeArtifact
	client           *http.Client
	directoryManager localfs.DirectoryManager
	logger           *chamberLogging.SlogLogger
}

type option func(*Runtime)

type runtimeArtifact struct {
	version string
	url     string
	sha256  string
}

func withHTTPClient(client *http.Client) option {
	return func(runtime *Runtime) {
		if client != nil {
			runtime.client = client
		}
	}
}

func withArtifact(artifact runtimeArtifact) option {
	return func(runtime *Runtime) {
		runtime.artifact = artifact
	}
}

func New(ctx context.Context, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager) (*Runtime, error) {
	return newWithOptions(ctx, config, directoryManager)
}

func newWithOptions(ctx context.Context, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager, options ...option) (*Runtime, error) {
	logger, err := chamberLogging.LoggerFromConfig(config.Logging, nil)
	if err != nil {
		return nil, err
	}

	artifact, err := defaultRuntimeArtifact(goruntime.GOARCH)
	if err != nil {
		return nil, err
	}
	runtime := &Runtime{
		config:           config,
		artifact:         artifact,
		client:           http.DefaultClient,
		directoryManager: directoryManager,
		logger:           logger,
	}
	for _, option := range options {
		option(runtime)
	}
	binary, err := configuredBinary(config)
	if err != nil {
		return nil, err
	}
	runtime.binary = binary
	if err := runtime.installBinary(ctx); err != nil {
		return nil, err
	}

	return runtime, nil
}

func (r *Runtime) Descriptor() chamberRuntimeShared.Descriptor {
	version := defaultVersion
	if r != nil {
		if r.artifact.version != "" {
			version = r.artifact.version
		}
	}
	return chamberRuntimeShared.Descriptor{
		Name:         runtimeName,
		Version:      version,
		Capabilities: cloneCapabilities(capabilities),
	}
}

func cloneCapabilities(capabilities chamberRuntimeShared.Capabilities) chamberRuntimeShared.Capabilities {
	return chamberRuntimeShared.Capabilities{
		Privileges: append([]capability.Privilege(nil), capabilities.Privileges...),
		Isolation:  append([]chamberRuntimeShared.Isolation(nil), capabilities.Isolation...),
	}
}

func (r *Runtime) Binary() chamberRuntimeShared.Binary {
	if r == nil {
		return chamberRuntimeShared.Binary{}
	}
	return r.binary
}

func (r *Runtime) installBinary(ctx context.Context) error {
	if r == nil || r.directoryManager == nil {
		return fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	artifact := r.artifact
	if artifact.version == "" || artifact.url == "" || artifact.sha256 == "" {
		return fmt.Errorf("%w: runc runtime requires version, url, and sha256", chamberErrors.ErrInvalidRequest)
	}
	expectedDigest, err := parseSHA256(artifact.sha256)
	if err != nil {
		return err
	}

	binary := r.binary
	binDir := filepath.Dir(binary.Path)

	if ok, err := fileMatchesSHA256(binary.Path, expectedDigest); err != nil {
		return fmt.Errorf("verify existing runtime binary: %w", err)
	} else if ok {
		chamberLogging.InfoWith(r.logger, ctx, "runtime binary ready",
			"runtime", binary.Name,
			"version", artifact.version,
			"path", binary.Path,
			"source", "cache",
		)
		return nil
	}

	chamberLogging.InfoWith(r.logger, ctx, "downloading runtime binary",
		"runtime", binary.Name,
		"version", artifact.version,
		"url", artifact.url,
		"path", binary.Path,
	)
	if err := r.download(ctx, artifact.url, expectedDigest, binDir, binary.Path); err != nil {
		return err
	}

	chamberLogging.InfoWith(r.logger, ctx, "runtime binary ready",
		"runtime", binary.Name,
		"version", artifact.version,
		"path", binary.Path,
		"source", "download",
	)
	return nil
}

func (r *Runtime) Run(ctx context.Context, request chamberRuntimeShared.RunRequest) (chamberRuntimeShared.Process, error) {
	if request.Bundle.BundlePath == "" {
		return nil, fmt.Errorf("%w: runtime bundle path is required", chamberErrors.ErrInvalidRequest)
	}
	containerID := request.Bundle.ContainerID
	if err := containerid.Validate(containerID); err != nil {
		return nil, err
	}
	binary := r.binary
	if binary.Path == "" {
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

	chamberLogging.InfoWith(r.logger, ctx, "started runtime container",
		"container_id", containerID,
		"pid", cmd.Process.Pid,
	)
	return newRuncProcess(cmd, stdout, stderr), nil
}

func (r *Runtime) ReadLog(containerID string, stream chamberRuntimeShared.LogStream) ([]byte, error) {
	path, err := r.logPath(containerID, stream)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (r *Runtime) State(ctx context.Context, containerID string) (chamberRuntimeShared.ContainerState, error) {
	if err := containerid.Validate(containerID); err != nil {
		return chamberRuntimeShared.ContainerState{}, err
	}
	binary, stateRoot, err := r.binaryAndStateRoot()
	if err != nil {
		return chamberRuntimeShared.ContainerState{}, err
	}
	state, err := readRuncState(ctx, binary.Path, stateRoot, containerID)
	if err != nil {
		return chamberRuntimeShared.ContainerState{}, err
	}
	return chamberRuntimeShared.ContainerState{
		ContainerID: containerID,
		Status:      chamberRuntimeShared.ContainerStatus(state.Status),
	}, nil
}

func (r *Runtime) Signal(ctx context.Context, request chamberRuntimeShared.SignalRequest) error {
	if err := containerid.Validate(request.ContainerID); err != nil {
		return err
	}
	if strings.TrimSpace(string(request.Signal)) == "" {
		return fmt.Errorf("%w: runtime signal is required", chamberErrors.ErrInvalidRequest)
	}
	if !chamberRuntimeShared.IsSupportedSignal(request.Signal) {
		return fmt.Errorf("%w: unsupported runtime signal %q", chamberErrors.ErrInvalidRequest, request.Signal)
	}
	binary, stateRoot, err := r.binaryAndStateRoot()
	if err != nil {
		return err
	}
	chamberLogging.InfoWith(r.logger, ctx, "signaling runtime container",
		"container_id", request.ContainerID,
		"signal", request.Signal,
	)
	cmd := exec.CommandContext(ctx, binary.Path, "--root", stateRoot, "kill", request.ContainerID, string(request.Signal))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("signal runc container %q: %w: %s", request.ContainerID, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r *Runtime) Delete(ctx context.Context, request chamberRuntimeShared.DeleteRequest) error {
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
	chamberLogging.InfoWith(r.logger, ctx, "deleting runtime container",
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
	if r.directoryManager == nil {
		return nil, nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if err := r.directoryManager.MkdirPrivate(logDir); err != nil {
		return nil, nil, fmt.Errorf("create runc log directory: %w", err)
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
		return nil, nil, fmt.Errorf("open stdout log: %w", err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("open stderr log: %w", err)
	}
	return stdout, stderr, nil
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

func (r *Runtime) binaryAndStateRoot() (chamberRuntimeShared.Binary, string, error) {
	binary := r.binary
	if binary.Path == "" {
		return chamberRuntimeShared.Binary{}, "", fmt.Errorf("%w: runtime binary is required", chamberErrors.ErrInvalidRequest)
	}
	stateRoot, err := r.stateRoot()
	if err != nil {
		return chamberRuntimeShared.Binary{}, "", err
	}
	return binary, stateRoot, nil
}

func (r *Runtime) stateRoot() (string, error) {
	if r.config.RuntimeRoot == "" {
		return "", fmt.Errorf("%w: runtime root is required", chamberErrors.ErrInvalidRequest)
	}
	runtimeRoot, err := absPath(r.config.RuntimeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve runtime root: %w", err)
	}
	return runtimeRoot, nil
}

func configuredBinary(config chamberRuntimeShared.Config) (chamberRuntimeShared.Binary, error) {
	if config.RuntimeBinDir == "" {
		return chamberRuntimeShared.Binary{}, fmt.Errorf("%w: runtime bin dir is required", chamberErrors.ErrInvalidRequest)
	}
	binDir, err := absPath(config.RuntimeBinDir)
	if err != nil {
		return chamberRuntimeShared.Binary{}, fmt.Errorf("resolve runtime bin dir: %w", err)
	}
	return chamberRuntimeShared.Binary{
		Name: runtimeName,
		Path: filepath.Join(binDir, runtimeName),
	}, nil
}

func defaultRuntimeArtifact(arch string) (runtimeArtifact, error) {
	switch arch {
	case "amd64":
		return runtimeArtifact{version: defaultVersion, url: defaultAMD64URL, sha256: defaultAMD64SHA256}, nil
	case "arm64":
		return runtimeArtifact{version: defaultVersion, url: defaultARM64URL, sha256: defaultARM64SHA256}, nil
	default:
		return runtimeArtifact{}, fmt.Errorf("%w: runc runtime does not have a default artifact for architecture %q", chamberErrors.ErrInvalidRequest, arch)
	}
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
