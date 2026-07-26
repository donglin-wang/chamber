// Package buildah contains Buildah-backed Dockerfile build mechanics for
// Chamber's image store.
package buildah

import (
	"bytes"
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
	"syscall"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

const (
	defaultStorageDriver = "vfs"
	defaultIsolation     = "chroot"
	protocolVersion      = 1

	defaultVersion     = "v0.1.0-beta.4"
	defaultAMD64URL    = "https://github.com/donglin-wang/chamber/releases/download/v0.1.0-beta.4/buildah-worker-linux-amd64"
	defaultAMD64SHA256 = "649dea3351658956bbca0a635baca721968f4cb37b20db8a344d817ac7415bcd"
	defaultARM64URL    = "https://github.com/donglin-wang/chamber/releases/download/v0.1.0-beta.4/buildah-worker-linux-arm64"
	defaultARM64SHA256 = "62ec2157a1783aad5fdb9916774c424729786e8827c22ce8acbd268a7fa10402"
)

type Builder struct {
	config           chamberImage.BuildahConfig
	root             string
	workerPath       string
	graphRoot        string
	runRoot          string
	tempRoot         string
	client           *http.Client
	directoryManager localfs.DirectoryManager
}

// New returns a builder with its private Buildah roots prepared and worker
// binary installed or verified.
func New(ctx context.Context, config chamberImage.BuildahConfig, root string, directoryManager localfs.DirectoryManager) (Builder, error) {
	builder := Builder{
		config:           config,
		root:             root,
		client:           http.DefaultClient,
		directoryManager: directoryManager,
	}
	if err := builder.prepare(ctx); err != nil {
		return Builder{}, err
	}
	return builder, nil
}

func (b Builder) Build(ctx context.Context, request chamberImage.BuildRequest, outputLayout string) error {
	if err := validateBuildContext(ctx); err != nil {
		return err
	}
	build, err := resolveRequest(request)
	if err != nil {
		return err
	}
	archivePath, cleanup, err := b.createOutputArchive(outputLayout)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := b.runBuildahWorker(ctx, build, archivePath); err != nil {
		return err
	}
	return b.importWorkerArchive(ctx, archivePath, outputLayout)
}

// ValidateRequest checks per-build inputs without preparing or running Buildah.
func ValidateRequest(request chamberImage.BuildRequest) error {
	_, err := resolveRequest(request)
	return err
}

func validateBuildContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: image build canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	return nil
}

func (b *Builder) prepare(ctx context.Context) error {
	if err := b.validate(ctx); err != nil {
		return err
	}
	if err := b.prepareBuildahRoots(); err != nil {
		return err
	}
	workerPath, err := b.prepareWorker(ctx)
	if err != nil {
		return err
	}
	b.workerPath = workerPath
	return nil
}

func (b Builder) validate(ctx context.Context) error {
	if err := validateBuildContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(b.root) == "" {
		return fmt.Errorf("%w: Buildah root is required", chamberErrors.ErrInvalidRequest)
	}
	if !filepath.IsAbs(b.root) {
		return fmt.Errorf("%w: Buildah root must be absolute", chamberErrors.ErrInvalidRequest)
	}
	if b.directoryManager == nil {
		return fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	return nil
}

func (b *Builder) prepareBuildahRoots() error {
	b.graphRoot = filepath.Join(b.root, "buildah-root")
	b.runRoot = filepath.Join(b.root, "buildah-runroot")
	b.tempRoot = filepath.Join(b.root, "buildah-tmp")
	for _, path := range []string{b.graphRoot, b.runRoot, b.tempRoot} {
		if err := b.directoryManager.MkdirPrivate(path); err != nil {
			return fmt.Errorf("%w: create Buildah storage directory %q: %w", chamberErrors.ErrFilesystemFailed, path, err)
		}
	}
	return nil
}

func (b Builder) createOutputArchive(outputLayout string) (string, func(), error) {
	tarPath, err := b.directoryManager.CreateTemp(filepath.Dir(outputLayout), ".buildah-output-*.tar")
	if err != nil {
		return "", nil, fmt.Errorf("%w: create temporary OCI archive: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	tarName := tarPath.Name()
	cleanup := func() {
		_ = os.Remove(tarName)
	}
	if err := tarPath.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%w: close temporary OCI archive: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	return tarName, cleanup, nil
}

func (b Builder) runBuildahWorker(ctx context.Context, build resolvedRequest, archivePath string) error {
	var logs bytes.Buffer
	var response bytes.Buffer
	command, err := b.buildWorkerCommand(ctx, build, archivePath)
	if err != nil {
		return err
	}
	command.Stdout = &response
	command.Stderr = &logs
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: image build canceled: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		message := strings.TrimSpace(logs.String())
		if message != "" {
			return fmt.Errorf("%w: run Buildah worker: %w: %s", chamberErrors.ErrBuildFailed, err, message)
		}
		return fmt.Errorf("%w: run Buildah worker: %w", chamberErrors.ErrBuildFailed, err)
	}
	return verifyWorkerResponse(response.Bytes(), logs.String())
}

func (b Builder) importWorkerArchive(ctx context.Context, archivePath string, outputLayout string) error {
	if err := b.extractOCITar(archivePath, outputLayout); err != nil {
		return err
	}
	if err := chamberImage.ValidateLayoutContext(ctx, outputLayout); err != nil {
		if errors.Is(err, chamberErrors.ErrCanceled) {
			return fmt.Errorf("%w: image build canceled while verifying generated OCI image layout: %w", chamberErrors.ErrCanceled, err)
		}
		return fmt.Errorf("%w: verify generated OCI image layout: %w", chamberErrors.ErrBuildFailed, err)
	}
	return nil
}

func (b Builder) buildWorkerCommand(ctx context.Context, build resolvedRequest, archivePath string) (*exec.Cmd, error) {
	if strings.TrimSpace(b.workerPath) == "" || strings.TrimSpace(b.graphRoot) == "" || strings.TrimSpace(b.runRoot) == "" || strings.TrimSpace(b.tempRoot) == "" {
		return nil, fmt.Errorf("%w: Buildah builder is not prepared", chamberErrors.ErrInvalidRequest)
	}
	storageDriver := strings.TrimSpace(b.config.StorageDriver)
	if storageDriver == "" {
		storageDriver = defaultStorageDriver
	}
	isolation := strings.TrimSpace(b.config.Isolation)
	if isolation == "" {
		isolation = defaultIsolation
	}
	payload, err := json.Marshal(workerBuildRequest{
		ProtocolVersion: protocolVersion,
		Operation:       "build",
		Reference:       build.reference,
		ContextPath:     build.contextPath,
		DockerfilePath:  build.dockerfilePath,
		OutputArchive:   archivePath,
		GraphRoot:       b.graphRoot,
		RunRoot:         b.runRoot,
		StorageDriver:   storageDriver,
		Runtime:         strings.TrimSpace(b.config.Runtime),
		Isolation:       isolation,
		Platform:        build.platform,
		Target:          build.target,
		BuildArgs:       build.buildArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode Buildah worker request: %w", chamberErrors.ErrBuildFailed, err)
	}
	command := exec.CommandContext(ctx, b.workerPath)
	command.Dir = build.contextPath
	command.Stdin = bytes.NewReader(payload)
	command.Env = append(os.Environ(), "TMPDIR="+b.tempRoot, "XDG_RUNTIME_DIR="+b.runRoot)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 5 * time.Second
	return command, nil
}

type workerBuildRequest struct {
	ProtocolVersion int                   `json:"protocol_version"`
	Operation       string                `json:"operation,omitempty"`
	Reference       string                `json:"reference"`
	ContextPath     string                `json:"context_path"`
	DockerfilePath  string                `json:"dockerfile_path"`
	OutputArchive   string                `json:"output_archive"`
	GraphRoot       string                `json:"graph_root"`
	RunRoot         string                `json:"run_root"`
	StorageDriver   string                `json:"storage_driver"`
	Runtime         string                `json:"runtime,omitempty"`
	Isolation       string                `json:"isolation"`
	Platform        chamberImage.Platform `json:"platform"`
	Target          string                `json:"target,omitempty"`
	BuildArgs       map[string]string     `json:"build_args,omitempty"`
}

type workerBuildResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	Error           string `json:"error,omitempty"`
}

func verifyWorkerResponse(content []byte, logs string) error {
	response, err := decodeWorkerResponse(content)
	if err != nil {
		message := strings.TrimSpace(logs)
		if message != "" {
			return fmt.Errorf("%w: decode Buildah worker response: %w: %s", chamberErrors.ErrBuildFailed, err, message)
		}
		return fmt.Errorf("%w: decode Buildah worker response: %w", chamberErrors.ErrBuildFailed, err)
	}
	if strings.TrimSpace(response.Error) != "" {
		return fmt.Errorf("%w: Buildah worker: %s", chamberErrors.ErrBuildFailed, strings.TrimSpace(response.Error))
	}
	if response.ProtocolVersion != protocolVersion {
		return fmt.Errorf("%w: Buildah worker protocol version %d, want %d", chamberErrors.ErrBuildFailed, response.ProtocolVersion, protocolVersion)
	}
	return nil
}

func decodeWorkerResponse(content []byte) (workerBuildResponse, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return workerBuildResponse{}, errors.New("empty response")
	}
	var response workerBuildResponse
	if err := json.Unmarshal(content, &response); err != nil {
		return workerBuildResponse{}, err
	}
	return response, nil
}

func (b Builder) prepareWorker(ctx context.Context) (string, error) {
	if localPath := strings.TrimSpace(b.config.Path); localPath != "" {
		return b.prepareLocalWorker(ctx, localPath)
	}
	return b.prepareManagedWorker(ctx)
}

func (b Builder) prepareLocalWorker(ctx context.Context, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: Buildah worker path must be absolute", chamberErrors.ErrInvalidRequest)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: Buildah worker path does not exist", chamberErrors.ErrInvalidRequest)
		}
		return "", fmt.Errorf("%w: inspect Buildah worker path: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: Buildah worker path must be a file", chamberErrors.ErrInvalidRequest)
	}
	if err := os.Chmod(path, 0755); err != nil {
		return "", fmt.Errorf("%w: make local Buildah worker executable: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	if err := b.probeWorker(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

type buildahBinary struct {
	version string
	url     string
	sha256  string
}

var runtimeArch = func() string {
	return goruntime.GOARCH
}

func defaultManagedWorkerPath(root string) string {
	return filepath.Join(root, "bin", "buildah-worker")
}

func managedBinary(config chamberImage.BuildahConfig) (buildahBinary, error) {
	version := strings.TrimSpace(config.Version)
	url := strings.TrimSpace(config.URL)
	sha256 := strings.TrimSpace(config.SHA256)
	if version != "" || url != "" || sha256 != "" {
		if version == "" || url == "" || sha256 == "" {
			return buildahBinary{}, fmt.Errorf("%w: Buildah worker managed download requires version, url, and sha256", chamberErrors.ErrInvalidRequest)
		}
		return buildahBinary{version: version, url: url, sha256: sha256}, nil
	}
	switch runtimeArch() {
	case "amd64":
		return buildahBinary{version: defaultVersion, url: defaultAMD64URL, sha256: defaultAMD64SHA256}, nil
	case "arm64":
		return buildahBinary{version: defaultVersion, url: defaultARM64URL, sha256: defaultARM64SHA256}, nil
	default:
		return buildahBinary{}, fmt.Errorf("%w: Buildah worker does not have a default binary for architecture %q", chamberErrors.ErrUnsupportedHost, runtimeArch())
	}
}

func (b Builder) prepareManagedWorker(ctx context.Context) (string, error) {
	binaryPath := defaultManagedWorkerPath(b.root)
	if err := b.directoryManager.MkdirPrivate(filepath.Dir(binaryPath)); err != nil {
		return "", fmt.Errorf("%w: create Buildah worker directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	binary, err := managedBinary(b.config)
	if err != nil {
		return "", err
	}
	expectedDigest, hasExpectedDigest, err := parseOptionalSHA256(binary.sha256)
	if err != nil {
		return "", err
	}
	if !hasExpectedDigest {
		return "", fmt.Errorf("%w: Buildah worker sha256 is required", chamberErrors.ErrInvalidRequest)
	}
	if ok, err := fileMatchesSHA256(binaryPath, expectedDigest); err != nil {
		return "", fmt.Errorf("%w: verify existing Buildah worker: %w", chamberErrors.ErrBuildInstallFailed, err)
	} else if ok {
		if err := os.Chmod(binaryPath, 0755); err != nil {
			return "", fmt.Errorf("%w: make existing Buildah worker executable: %w", chamberErrors.ErrBuildInstallFailed, err)
		}
		if err := b.probeWorker(ctx, binaryPath); err != nil {
			return "", err
		}
		return binaryPath, nil
	}

	if err := b.downloadBuildahWorker(ctx, binary.url, expectedDigest, binaryPath); err != nil {
		return "", err
	}
	if err := b.probeWorker(ctx, binaryPath); err != nil {
		return "", err
	}
	return binaryPath, nil
}

func (b Builder) probeWorker(ctx context.Context, binaryPath string) error {
	payload, err := json.Marshal(workerBuildRequest{
		ProtocolVersion: protocolVersion,
		Operation:       "probe",
	})
	if err != nil {
		return fmt.Errorf("%w: encode Buildah worker probe: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	command := exec.CommandContext(ctx, binaryPath)
	command.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: Buildah worker probe canceled: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("%w: probe Buildah worker: %w: %s", chamberErrors.ErrBuildInstallFailed, err, message)
		}
		return fmt.Errorf("%w: probe Buildah worker: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	response, err := decodeWorkerResponse(stdout.Bytes())
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("%w: decode Buildah worker probe response: %w: %s", chamberErrors.ErrBuildInstallFailed, err, message)
		}
		return fmt.Errorf("%w: decode Buildah worker probe response: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	if strings.TrimSpace(response.Error) != "" {
		return fmt.Errorf("%w: Buildah worker probe: %s", chamberErrors.ErrBuildInstallFailed, strings.TrimSpace(response.Error))
	}
	if response.ProtocolVersion != protocolVersion {
		return fmt.Errorf("%w: Buildah worker protocol version %d, want %d", chamberErrors.ErrBuildInstallFailed, response.ProtocolVersion, protocolVersion)
	}
	return nil
}

type resolvedRequest struct {
	reference      string
	contextPath    string
	dockerfilePath string
	platform       chamberImage.Platform
	target         string
	buildArgs      map[string]string
}

func resolveRequest(request chamberImage.BuildRequest) (resolvedRequest, error) {
	reference, err := chamberImage.CanonicalImageReference(request.Reference)
	if err != nil {
		return resolvedRequest{}, err
	}

	contextPath := strings.TrimSpace(request.ContextPath)
	if contextPath == "" {
		return resolvedRequest{}, fmt.Errorf("%w: build context path is required", chamberErrors.ErrInvalidRequest)
	}
	if !filepath.IsAbs(contextPath) {
		return resolvedRequest{}, fmt.Errorf("%w: build context path must be absolute", chamberErrors.ErrInvalidRequest)
	}
	contextInfo, err := os.Stat(contextPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resolvedRequest{}, fmt.Errorf("%w: build context path does not exist", chamberErrors.ErrInvalidRequest)
		}
		return resolvedRequest{}, fmt.Errorf("%w: inspect build context path: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if !contextInfo.IsDir() {
		return resolvedRequest{}, fmt.Errorf("%w: build context path must be a directory", chamberErrors.ErrInvalidRequest)
	}
	contextPath, err = filepath.EvalSymlinks(contextPath)
	if err != nil {
		return resolvedRequest{}, fmt.Errorf("%w: resolve build context path: %w", chamberErrors.ErrFilesystemFailed, err)
	}

	dockerfilePath := strings.TrimSpace(request.DockerfilePath)
	if dockerfilePath == "" {
		dockerfilePath = filepath.Join(contextPath, "Dockerfile")
	}
	if !filepath.IsAbs(dockerfilePath) {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must be absolute", chamberErrors.ErrInvalidRequest)
	}
	if !pathContains(contextPath, dockerfilePath) {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must be inside the build context", chamberErrors.ErrInvalidRequest)
	}
	dockerfileInfo, err := os.Stat(dockerfilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path does not exist", chamberErrors.ErrInvalidRequest)
		}
		return resolvedRequest{}, fmt.Errorf("%w: inspect Dockerfile path: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if dockerfileInfo.IsDir() {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must be a file", chamberErrors.ErrInvalidRequest)
	}
	dockerfilePath, err = filepath.EvalSymlinks(dockerfilePath)
	if err != nil {
		return resolvedRequest{}, fmt.Errorf("%w: resolve Dockerfile path: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	if !pathContains(contextPath, dockerfilePath) {
		return resolvedRequest{}, fmt.Errorf("%w: Dockerfile path must resolve inside the build context", chamberErrors.ErrInvalidRequest)
	}

	buildArgs := make(map[string]string, len(request.BuildArgs))
	for key, value := range request.BuildArgs {
		key = strings.TrimSpace(key)
		if key == "" {
			return resolvedRequest{}, fmt.Errorf("%w: build argument name is required", chamberErrors.ErrInvalidRequest)
		}
		buildArgs[key] = value
	}
	return resolvedRequest{
		reference:      reference,
		contextPath:    contextPath,
		dockerfilePath: dockerfilePath,
		platform:       chamberImage.NormalizePlatform(request.Platform),
		target:         strings.TrimSpace(request.Target),
		buildArgs:      buildArgs,
	}, nil
}

func pathContains(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func (b Builder) downloadBuildahWorker(ctx context.Context, url string, expectedDigest []byte, binaryPath string) error {
	client := b.client
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: create Buildah worker download request: %w", chamberErrors.ErrBuildInstallFailed, err)
	}

	response, err := client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: Buildah worker download canceled while requesting: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: download Buildah worker: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: download Buildah worker: unexpected HTTP status %s", chamberErrors.ErrBuildInstallFailed, response.Status)
	}

	tmp, err := b.directoryManager.CreateTemp(filepath.Dir(binaryPath), "."+filepath.Base(binaryPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary Buildah worker: %w", chamberErrors.ErrFilesystemFailed, err)
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
			return fmt.Errorf("%w: Buildah worker download canceled while reading response: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return fmt.Errorf("%w: download Buildah worker: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	actualDigest := digest.Sum(nil)
	if !equalDigest(actualDigest, expectedDigest) {
		_ = tmp.Close()
		return fmt.Errorf("%w: verify Buildah worker checksum: got %s, want %s", chamberErrors.ErrBuildInstallFailed, hex.EncodeToString(actualDigest), hex.EncodeToString(expectedDigest))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: sync Buildah worker: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: set Buildah worker mode: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: close Buildah worker: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		return fmt.Errorf("%w: commit Buildah worker: %w", chamberErrors.ErrBuildInstallFailed, err)
	}
	committed = true
	return nil
}

func parseOptionalSHA256(value string) ([]byte, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false, nil
	}
	digest, err := hex.DecodeString(value)
	if err != nil {
		return nil, false, fmt.Errorf("%w: parse Buildah worker sha256: %w", chamberErrors.ErrInvalidRequest, err)
	}
	if len(digest) != sha256.Size {
		return nil, false, fmt.Errorf("%w: Buildah worker sha256 must decode to %d bytes", chamberErrors.ErrInvalidRequest, sha256.Size)
	}
	return digest, true, nil
}

func fileMatchesSHA256(path string, expectedDigest []byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
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

func equalDigest(actual []byte, expected []byte) bool {
	if len(actual) != len(expected) {
		return false
	}
	var diff byte
	for i := range actual {
		diff |= actual[i] ^ expected[i]
	}
	return diff == 0
}
