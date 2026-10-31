package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/logging"
)

const DefaultImage = "docker.io/library/golang:1.26.4-bookworm"
const ContainerGoStateRoot = "/go/chamber-ci"

const GoTestShellCommand = "mkdir -p " + ContainerGoStateRoot + "/build " + ContainerGoStateRoot + "/mod " + ContainerGoStateRoot + "/work && " +
	"GOCACHE=" + ContainerGoStateRoot + "/build " +
	"GOMODCACHE=" + ContainerGoStateRoot + "/mod " +
	"GOTMPDIR=" + ContainerGoStateRoot + "/work " +
	"exec go test ./..."

func GoTestCommand() []string {
	return []string{"/bin/sh", "-c", GoTestShellCommand}
}

type Name string

const (
	DirectSDK Name = "direct-sdk"
)

type Config struct {
	Root        string
	Workdir     string
	Image       string
	EvidenceDir string
	Timeout     time.Duration
	Keep        bool
	Stdout      []io.Writer
	Stderr      []io.Writer
}

func Names() []Name {
	return []Name{DirectSDK}
}

func Run(ctx context.Context, cfg Config) (int, error) {
	if strings.TrimSpace(cfg.EvidenceDir) != "" {
		cfg.EvidenceDir = filepath.Join(cfg.EvidenceDir, string(DirectSDK))
	}
	return RunDirectSDK(ctx, cfg)
}

func RunDirectSDK(ctx context.Context, cfg Config) (int, error) {
	ctx, cancel, cfg, err := prepareRun(ctx, cfg)
	if err != nil {
		return 1, err
	}
	defer cancel()

	workspace, err := workspacePath(cfg)
	if err != nil {
		return 1, err
	}
	root, cleanup, err := createRunRoot(ctx, cfg)
	if err != nil {
		return 1, err
	}
	defer cleanup()

	logging.Info(ctx, "CI root ready", "root", root, "pipeline", DirectSDK)
	imageConfig, imageWorkspace, err := imageStoreConfig(root)
	if err != nil {
		return 1, err
	}
	imageStore, err := chamberImageFactory.NewStoreWithWorkspace(imageConfig, imageWorkspace)
	if err != nil {
		return 1, fmt.Errorf("create image store: %w", err)
	}

	logging.Info(ctx, "CI image pull started", "image_ref", cfg.Image)
	image, err := imageStore.Pull(ctx, chamberImage.PullRequest{
		Reference: cfg.Image,
		Platform:  chamberImage.Platform{OS: "linux"},
	})
	if err != nil {
		return 1, fmt.Errorf("pull image %q: %w", cfg.Image, err)
	}
	logging.Info(ctx, "CI image pulled", "image_ref", image.Reference, "digest", image.Digest, "bytes", image.SizeBytes)
	runtimeConfig, runtimeWorkspace, runtimeBinaryWorkspace, err := runtimeConfig(root)
	if err != nil {
		return 1, err
	}
	runtime, err := chamberRuntimeFactory.NewRuntimeWithWorkspace(ctx, runtimeConfig, runtimeWorkspace, runtimeBinaryWorkspace)
	if err != nil {
		return 1, fmt.Errorf("create runtime: %w", err)
	}
	descriptor := runtime.Descriptor()
	logging.Info(ctx, "CI runtime ready", "runtime", descriptor.Name, "version", descriptor.Version, "path", descriptor.BinaryPath)

	container, err := runtime.RunImage(ctx, image, chamberRuntime.RunImageRequest{
		Process: chamberBundle.ProcessSpec{
			Args: GoTestCommand(),
			Cwd:  "/workspace",
		},
		Mounts: []chamberBundle.Mount{
			{Source: workspace, Target: "/workspace"},
		},
		Stdout: cfg.Stdout,
		Stderr: cfg.Stderr,
	})
	if err != nil {
		return 1, fmt.Errorf("run CI container: %w", err)
	}
	result, waitErr := container.Wait(ctx)
	exitCode := ContainerExitCode(result)
	stdout, stdoutErr := container.ReadLog(chamberRuntime.StdoutLogStream)
	stderr, stderrErr := container.ReadLog(chamberRuntime.StderrLogStream)
	if len(stdout) > 0 {
		logging.Info(ctx, "CI output", "stream", "stdout", "output", string(stdout))
	}
	if len(stderr) > 0 {
		logging.Info(ctx, "CI output", "stream", "stderr", "output", string(stderr))
	}
	if deleteErr := container.Delete(context.Background(), true); deleteErr != nil && waitErr == nil && !looksAlreadyDeleted(deleteErr) {
		waitErr = fmt.Errorf("delete runtime container: %w", deleteErr)
	}
	if !cfg.Keep {
		_ = container.DeleteLog(chamberRuntime.StdoutLogStream)
		_ = container.DeleteLog(chamberRuntime.StderrLogStream)
	}
	if waitErr != nil {
		return exitCode, waitErr
	}
	if stdoutErr != nil {
		return exitCode, fmt.Errorf("read stdout: %w", stdoutErr)
	}
	if stderrErr != nil {
		return exitCode, fmt.Errorf("read stderr: %w", stderrErr)
	}
	if exitCode == 0 {
		logging.Info(ctx, "CI passed", "pipeline", DirectSDK)
	} else {
		logging.Error(ctx, "CI failed", "pipeline", DirectSDK, "exit_code", exitCode)
	}
	return exitCode, nil
}

func ContainerExitCode(result chamberRuntime.ContainerResult) int {
	if result.ExitCode == nil {
		return 1
	}
	return *result.ExitCode
}

func prepareRun(ctx context.Context, cfg Config) (context.Context, context.CancelFunc, Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cancel := func() {}
	if cfg.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
	}
	if strings.TrimSpace(cfg.Image) == "" {
		cfg.Image = DefaultImage
	}
	if strings.TrimSpace(cfg.Workdir) == "" {
		cfg.Workdir = "."
	}
	if strings.TrimSpace(cfg.Root) == "" {
		cancel()
		return ctx, func() {}, cfg, fmt.Errorf("CI root is required")
	}
	return ctx, cancel, cfg, nil
}

func workspacePath(cfg Config) (string, error) {
	workspace, err := filepath.Abs(cfg.Workdir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	rootParent, err := filepath.Abs(cfg.Root)
	if err != nil {
		return "", fmt.Errorf("resolve CI root: %w", err)
	}
	if PathContains(workspace, rootParent) {
		return "", fmt.Errorf("CI root %q must be outside workspace %q", rootParent, workspace)
	}
	return workspace, nil
}

func createRunRoot(ctx context.Context, cfg Config) (string, func(), error) {
	rootParent, err := filepath.Abs(cfg.Root)
	if err != nil {
		return "", func() {}, fmt.Errorf("resolve CI root: %w", err)
	}
	ciWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    rootParent,
		TmpRoot: filepath.Join(rootParent, "tmp"),
		Requirements: hostfs.FeatureSet{
			PrivateDirs: true,
		},
	})
	if err != nil {
		return "", func() {}, fmt.Errorf("create CI workspace: %w", err)
	}
	root, err := ciWorkspace.MkdirTemp("runs", "chamber-ci-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create CI root: %w", err)
	}
	cleanup := func() {}
	if !cfg.Keep {
		cleanup = func() {
			if err := os.RemoveAll(root); err != nil {
				logging.Error(ctx, "remove CI root failed", "root", root, "error", err)
			}
		}
	}
	return root, cleanup, nil
}

func imageStoreConfig(root string) (chamberImage.Config, *hostfs.Workspace, error) {
	config := chamberImage.DefaultConfig(root)
	config.TmpRoot = filepath.Join(root, "tmp", "images")
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    config.Root,
		TmpRoot: config.TmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:           true,
			FileFsync:             true,
			AtomicFileRename:      true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		return chamberImage.Config{}, nil, fmt.Errorf("create image workspace: %w", err)
	}
	return config, workspace, nil
}

func runtimeConfig(root string) (chamberRuntime.Config, *hostfs.Workspace, *hostfs.Workspace, error) {
	config := chamberRuntime.DefaultConfig(root)
	config.RuntimeTmpRoot = filepath.Join(root, "tmp", "runtime")
	config.RuntimeBinTmpRoot = filepath.Join(root, "tmp", "runtime-bin")
	runtimeWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    config.RuntimeRoot,
		TmpRoot: config.RuntimeTmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		return chamberRuntime.Config{}, nil, nil, fmt.Errorf("create runtime workspace: %w", err)
	}
	binaryWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    config.RuntimeBinDir,
		TmpRoot: config.RuntimeBinTmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		return chamberRuntime.Config{}, nil, nil, fmt.Errorf("create runtime binary workspace: %w", err)
	}
	return config, runtimeWorkspace, binaryWorkspace, nil
}

func PathContains(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func looksAlreadyDeleted(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return errors.Is(err, os.ErrNotExist) ||
		strings.Contains(message, "does not exist") ||
		strings.Contains(message, "container does not exist")
}
