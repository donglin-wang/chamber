package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberBundleFactory "github.com/donglin-wang/chamber/pkg/bundle/factory"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/logging"
	"github.com/google/uuid"
)

const defaultCIImage = "docker.io/library/golang:1.26.4-bookworm"
const containerGoStateRoot = "/go/chamber-ci"

var testCommand = []string{
	"/bin/sh",
	"-c",
	"mkdir -p " + containerGoStateRoot + "/build " + containerGoStateRoot + "/mod " + containerGoStateRoot + "/work && " +
		"GOCACHE=" + containerGoStateRoot + "/build " +
		"GOMODCACHE=" + containerGoStateRoot + "/mod " +
		"GOTMPDIR=" + containerGoStateRoot + "/work " +
		"exec go test ./...",
}

type ciConfig struct {
	Root    string
	Workdir string
	Image   string
	Timeout time.Duration
	Keep    bool
}

func runCI(ctx context.Context, cfg ciConfig) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	if strings.TrimSpace(cfg.Image) == "" {
		cfg.Image = defaultCIImage
	}
	if strings.TrimSpace(cfg.Workdir) == "" {
		cfg.Workdir = "."
	}
	if strings.TrimSpace(cfg.Root) == "" {
		return 1, fmt.Errorf("CI root is required")
	}

	workspace, err := filepath.Abs(cfg.Workdir)
	if err != nil {
		return 1, fmt.Errorf("resolve workspace: %w", err)
	}
	rootParent, err := filepath.Abs(cfg.Root)
	if err != nil {
		return 1, fmt.Errorf("resolve CI root: %w", err)
	}
	if pathContains(workspace, rootParent) {
		return 1, fmt.Errorf("CI root %q must be outside workspace %q", rootParent, workspace)
	}
	ciWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    rootParent,
		TmpRoot: filepath.Join(rootParent, "tmp"),
		Requirements: hostfs.FeatureSet{
			PrivateDirs: true,
		},
	})
	if err != nil {
		return 1, fmt.Errorf("create CI workspace: %w", err)
	}
	root, err := ciWorkspace.MkdirTemp("runs", "chamber-ci-*")
	if err != nil {
		return 1, fmt.Errorf("create CI root: %w", err)
	}
	if !cfg.Keep {
		defer func() {
			if err := os.RemoveAll(root); err != nil {
				logging.Error(ctx, "remove CI root failed", "root", root, "error", err)
			}
		}()
	}

	logging.Info(ctx, "CI root ready", "root", root)
	imageConfig, imageWorkspace, err := imageStoreConfig(root)
	if err != nil {
		return 1, err
	}
	imageStore, err := chamberImageFactory.NewStore(imageConfig, imageWorkspace)
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
	imageLayout, err := imageStore.Layout(ctx)
	if err != nil {
		return 1, fmt.Errorf("read image layout: %w", err)
	}

	bundleConfig, bundleWorkspace, err := bundleProvisionerConfig(root)
	if err != nil {
		return 1, err
	}
	provisioner, err := chamberBundleFactory.NewProvisioner(bundleConfig, bundleWorkspace)
	if err != nil {
		return 1, fmt.Errorf("create bundle provisioner: %w", err)
	}

	runtimeConfig, runtimeWorkspace, runtimeBinaryWorkspace, err := runtimeConfig(root)
	if err != nil {
		return 1, err
	}
	runtime, err := chamberRuntimeFactory.NewRuntime(ctx, runtimeConfig, runtimeWorkspace, runtimeBinaryWorkspace)
	if err != nil {
		return 1, fmt.Errorf("create runtime: %w", err)
	}
	descriptor := runtime.Descriptor()
	logging.Info(ctx, "CI runtime ready", "runtime", descriptor.Name, "version", descriptor.Version, "path", descriptor.BinaryPath)

	terminal := false
	provisioned, err := provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
		ContainerID:   "chamber-ci-" + uuid.NewString(),
		ImageLayout:   imageLayout,
		ImageRef:      image.Reference,
		ImageDigest:   image.Digest,
		ImagePlatform: image.Platform,
		Process: chamberBundle.ProcessSpec{
			Args:     testCommand,
			Cwd:      "/workspace",
			Terminal: &terminal,
		},
		Mounts: []chamberBundle.Mount{
			{Source: workspace, Target: "/workspace"},
		},
	})
	if err != nil {
		return 1, fmt.Errorf("provision CI bundle: %w", err)
	}
	if !cfg.Keep {
		defer func() {
			if err := provisioner.Remove(context.Background(), provisioned); err != nil {
				logging.Error(ctx, "remove CI bundle failed", "bundle", provisioned.BundlePath, "error", err)
			}
		}()
	}

	container, err := runtime.Run(ctx, chamberRuntime.RunRequest{Bundle: provisioned})
	if err != nil {
		return 1, fmt.Errorf("run CI container: %w", err)
	}
	result, waitErr := container.Wait(ctx)
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
		return result.ExitCode, waitErr
	}
	if stdoutErr != nil {
		return result.ExitCode, fmt.Errorf("read stdout: %w", stdoutErr)
	}
	if stderrErr != nil {
		return result.ExitCode, fmt.Errorf("read stderr: %w", stderrErr)
	}
	if result.ExitCode == 0 {
		logging.Info(ctx, "CI passed")
	} else {
		logging.Error(ctx, "CI failed", "exit_code", result.ExitCode)
	}
	return result.ExitCode, nil
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

func bundleProvisionerConfig(root string) (chamberBundle.Config, *hostfs.Workspace, error) {
	config := chamberBundle.DefaultConfig(root)
	config.TmpRoot = filepath.Join(root, "tmp", "bundles")
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    config.Root,
		TmpRoot: config.TmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:           true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		return chamberBundle.Config{}, nil, fmt.Errorf("create bundle workspace: %w", err)
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

func pathContains(parent string, child string) bool {
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
