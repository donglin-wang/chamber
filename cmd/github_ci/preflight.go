package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/google/uuid"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberBundleFactory "github.com/donglin-wang/chamber/pkg/bundle/factory"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

const preflightImage = "docker.io/library/busybox:1.36.1"

func runPreflight(ctx context.Context, cfg config) error {
	if goruntime.GOOS != "linux" {
		return fmt.Errorf("github_ci requires Linux; current GOOS is %q", goruntime.GOOS)
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("github_ci must run as an unprivileged user")
	}
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve current user: %w", err)
	}
	if err := requireSubordinateIDMapping("/etc/subuid", current.Username); err != nil {
		return err
	}
	if err := requireSubordinateIDMapping("/etc/subgid", current.Username); err != nil {
		return err
	}
	for _, binary := range []string{"git", "newuidmap", "newgidmap"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("%s is required: %w", binary, err)
		}
	}
	if _, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    cfg.Root,
		TmpRoot: filepath.Join(cfg.Root, "tmp", "preflight"),
		Requirements: hostfs.FeatureSet{
			PrivateDirs: true,
		},
	}); err != nil {
		return fmt.Errorf("validate Chamber CI root: %w", err)
	}
	if err := runTinyContainerPreflight(ctx, cfg); err != nil {
		return fmt.Errorf("run tiny Chamber container preflight: %w", err)
	}
	return nil
}

func requireSubordinateIDMapping(path string, username string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	prefix := username + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return nil
		}
	}
	return fmt.Errorf("%s does not contain subordinate ID mapping for %q", path, username)
}

func runTinyContainerPreflight(ctx context.Context, cfg config) error {
	preflightRoot := filepath.Join(cfg.Root, "chamber-root")
	imageConfig := chamberImage.DefaultConfig(preflightRoot)
	imageConfig.TmpRoot = filepath.Join(preflightRoot, "tmp", "images")
	bundleConfig := chamberBundle.DefaultConfig(preflightRoot)
	bundleConfig.TmpRoot = filepath.Join(preflightRoot, "tmp", "bundles")
	runtimeConfig := chamberRuntime.DefaultConfig(preflightRoot)
	runtimeConfig.RuntimeTmpRoot = filepath.Join(preflightRoot, "tmp", "runtime")
	runtimeConfig.RuntimeBinTmpRoot = filepath.Join(preflightRoot, "tmp", "runtime-bin")

	imageWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    imageConfig.Root,
		TmpRoot: imageConfig.TmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:           true,
			FileFsync:             true,
			AtomicFileRename:      true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		return err
	}
	bundleWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    bundleConfig.Root,
		TmpRoot: bundleConfig.TmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:           true,
			AtomicDirectoryRename: true,
		},
	})
	if err != nil {
		return err
	}
	runtimeWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    runtimeConfig.RuntimeRoot,
		TmpRoot: runtimeConfig.RuntimeTmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		return err
	}
	binaryWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    runtimeConfig.RuntimeBinDir,
		TmpRoot: runtimeConfig.RuntimeBinTmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		return err
	}
	imageStore, err := chamberImageFactory.NewStore(imageConfig, imageWorkspace)
	if err != nil {
		return err
	}
	image, err := imageStore.Pull(ctx, chamberImage.PullRequest{
		Reference: preflightImage,
		Platform:  chamberImage.Platform{OS: "linux"},
	})
	if err != nil {
		return err
	}
	imageLayout, err := imageStore.Layout(ctx)
	if err != nil {
		return err
	}
	provisioner, err := chamberBundleFactory.NewProvisioner(bundleConfig, bundleWorkspace)
	if err != nil {
		return err
	}
	runtime, err := chamberRuntimeFactory.NewRuntime(ctx, runtimeConfig, runtimeWorkspace, binaryWorkspace)
	if err != nil {
		return err
	}
	terminal := false
	provisioned, err := provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
		ContainerID:   "chamber-ci-preflight-" + uuid.NewString(),
		ImageLayout:   imageLayout,
		ImageRef:      image.Reference,
		ImageDigest:   image.Digest,
		ImagePlatform: image.Platform,
		Process: chamberBundle.ProcessSpec{
			Args:     []string{"true"},
			Terminal: &terminal,
		},
	})
	if err != nil {
		return err
	}
	defer os.RemoveAll(provisioned.BundlePath)
	container, err := runtime.Run(ctx, chamberRuntime.RunRequest{Bundle: provisioned})
	if err != nil {
		return err
	}
	result, waitErr := container.Wait(ctx)
	if deleteErr := container.Delete(context.Background(), true); deleteErr != nil && waitErr == nil {
		waitErr = deleteErr
	}
	_ = container.DeleteLog(chamberRuntime.StdoutLogStream)
	_ = container.DeleteLog(chamberRuntime.StderrLogStream)
	if waitErr != nil {
		return waitErr
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("preflight container exited with code %d", result.ExitCode)
	}
	return nil
}
