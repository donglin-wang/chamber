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

	ciPipeline "github.com/donglin-wang/chamber/cmd/github-ci/pipeline"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/logging"
)

const preflightImage = "docker.io/library/busybox:1.36.1"

func runPreflight(ctx context.Context, cfg config) error {
	if goruntime.GOOS != "linux" {
		return fmt.Errorf("github-ci requires Linux; current GOOS is %q", goruntime.GOOS)
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("github-ci must run as an unprivileged user")
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
	for _, binary := range []string{"git", "go", "newuidmap", "newgidmap"} {
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
	runtimeConfig := chamberRuntime.DefaultConfig(preflightRoot)
	runtimeConfig.RuntimeTmpRoot = filepath.Join(preflightRoot, "tmp", "runtime")
	runtimeConfig.RuntimeBinTmpRoot = filepath.Join(preflightRoot, "tmp", "runtime-bin")

	imageStore, err := chamberImageFactory.NewStore(imageConfig)
	if err != nil {
		return err
	}
	image, err := imageStore.Pull(ctx, chamberImage.PullRequest{Reference: preflightImage})
	if err != nil {
		return err
	}
	runtime, err := chamberRuntimeFactory.NewRuntime(ctx, runtimeConfig)
	if err != nil {
		return err
	}
	container, err := runtime.RunImage(ctx, image, chamberRuntime.RunImageRequest{
		Process: chamberBundle.ProcessSpec{Args: []string{"true"}},
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := container.Delete(context.Background(), true); err != nil {
			logging.Error(ctx, "remove preflight container failed", "container_id", container.ID(), "error", err)
		}
		_ = container.DeleteLog(chamberRuntime.StdoutLogStream)
		_ = container.DeleteLog(chamberRuntime.StderrLogStream)
	}()
	result, err := container.Wait(ctx)
	if err != nil {
		return err
	}
	if exitCode := ciPipeline.ContainerExitCode(result); exitCode != 0 {
		return fmt.Errorf("preflight container exited with code %d", exitCode)
	}
	return nil
}
