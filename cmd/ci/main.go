package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageShared "github.com/donglin-wang/chamber/pkg/image/shared"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	"github.com/donglin-wang/chamber/pkg/shared/logging"
	"github.com/google/uuid"
)

type config struct {
	root     string
	workdir  string
	image    string
	timeout  time.Duration
	keep     bool
	exitCode int
}

type job struct {
	name string
	args []string
}

type jobResult struct {
	name     string
	exitCode int
	stdout   []byte
	stderr   []byte
	err      error
}

func main() {
	configureLogging()
	cfg := parseFlags()
	if err := run(&cfg); err != nil {
		logging.Error(context.Background(), "CI failed", "error", err)
		os.Exit(1)
	}
	os.Exit(cfg.exitCode)
}

func configureLogging() {
	logging.SetLogger(logging.NewJSONLogger(os.Stderr, slog.LevelInfo))
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.root, "root", "", "root directory for Chamber CI state; defaults outside the workspace")
	flag.StringVar(&cfg.workdir, "workdir", ".", "workspace directory to mount into the container")
	flag.StringVar(&cfg.image, "image", "docker.io/library/golang:1.26.4-bookworm", "OCI image used to run CI jobs")
	flag.DurationVar(&cfg.timeout, "timeout", 30*time.Minute, "timeout for the whole CI run")
	flag.BoolVar(&cfg.keep, "keep", false, "keep provisioned bundles after jobs finish")
	flag.Parse()
	return cfg
}

func run(cfg *config) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	loggingConfig := logging.DefaultConfig()

	workspace, err := filepath.Abs(cfg.workdir)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	root, err := resolveRoot(cfg.root, workspace)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}

	directoryManager := localfs.NewDirectoryManager()
	if err := directoryManager.MkdirPrivate(root); err != nil {
		return fmt.Errorf("create CI root: %w", err)
	}
	logging.Info(ctx, "CI root ready", "root", root)

	paths := ciPaths(root)
	for _, path := range []string{
		paths.goBuildCache,
		paths.goModCache,
	} {
		if err := directoryManager.MkdirPrivate(path); err != nil {
			return fmt.Errorf("create CI path %q: %w", path, err)
		}
	}

	runtime, err := chamberRuntime.NewRuntime(ctx, chamberRuntimeShared.Config{
		RuntimeRoot:   paths.runtimeRoot,
		RuntimeBinDir: paths.runtimeBinDir,
		Name:          chamberRuntimeShared.RuntimeNameRunc,
		Privilege:     capability.Rootless,
		Logging:       loggingConfig,
	}, directoryManager)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	descriptor := runtime.Descriptor()
	logging.Info(ctx, "CI runtime ready", "runtime", descriptor.Name, "version", descriptor.Version, "path", descriptor.BinaryPath)

	puller, err := chamberImage.NewPuller(chamberImageShared.Config{
		Root:    paths.imageRoot,
		Logging: loggingConfig,
	}, directoryManager)
	if err != nil {
		return fmt.Errorf("create image puller: %w", err)
	}
	imageLayout, err := ensureImage(ctx, puller, cfg.image)
	if err != nil {
		return err
	}

	provisioner, err := chamberBundle.NewProvisioner(
		chamberBundleShared.Config{
			Root:      paths.bundleRoot,
			Name:      chamberBundleShared.ProvisionerNameDirectory,
			Privilege: capability.Rootless,
			Logging:   loggingConfig,
		},
		directoryManager,
	)
	if err != nil {
		return fmt.Errorf("create bundle provisioner: %w", err)
	}

	jobs := []job{
		{name: "pkg", args: []string{"go", "test", "./pkg/..."}},
		{name: "full", args: []string{"go", "test", "./..."}},
	}

	results := make([]jobResult, 0, len(jobs))
	for _, job := range jobs {
		result := runJob(ctx, runtime, provisioner, jobRequest{
			job:          job,
			imageRef:     cfg.image,
			imageLayout:  imageLayout,
			workspace:    workspace,
			goBuildCache: paths.goBuildCache,
			goModCache:   paths.goModCache,
			keep:         cfg.keep,
		})
		results = append(results, result)
		logResult(ctx, result)
	}

	cfg.exitCode = finalExitCode(results)
	if cfg.exitCode != 0 {
		return nil
	}
	logging.Info(ctx, "CI passed")
	return nil
}

func resolveRoot(root string, workspace string) (string, error) {
	var resolved string
	var err error
	if strings.TrimSpace(root) == "" {
		cacheRoot, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache directory: %w", err)
		}
		resolved = filepath.Join(cacheRoot, "chamber", "ci")
	} else {
		resolved, err = filepath.Abs(root)
		if err != nil {
			return "", err
		}
	}

	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if pathContains(workspace, resolved) {
		return "", fmt.Errorf("CI root %q must be outside workspace %q", resolved, workspace)
	}
	return resolved, nil
}

func pathContains(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

type paths struct {
	imageRoot     string
	bundleRoot    string
	runtimeRoot   string
	runtimeBinDir string
	goBuildCache  string
	goModCache    string
}

func ciPaths(root string) paths {
	return paths{
		imageRoot:     filepath.Join(root, "images"),
		bundleRoot:    filepath.Join(root, "bundles"),
		runtimeRoot:   filepath.Join(root, "run", "runtime"),
		runtimeBinDir: filepath.Join(root, "bin"),
		goBuildCache:  filepath.Join(root, "cache", "go-build"),
		goModCache:    filepath.Join(root, "cache", "go-mod"),
	}
}

func ensureImage(ctx context.Context, puller chamberImageShared.Puller, imageRef string) (string, error) {
	logging.Info(ctx, "CI image pull started", "image_ref", imageRef)
	pulled, err := puller.Pull(ctx, chamberImageShared.PullRequest{
		Reference: imageRef,
		Platform: chamberImageShared.Platform{
			OS: "linux",
		},
	})
	if err != nil {
		return "", fmt.Errorf("pull image %q: %w", imageRef, err)
	}
	if pulled.LayoutPath == "" {
		return "", fmt.Errorf("pull image %q: image puller returned empty layout path", imageRef)
	}
	logging.Info(ctx, "CI image pulled", "image_ref", pulled.Reference, "digest", pulled.Digest, "bytes", pulled.SizeBytes)
	return pulled.LayoutPath, nil
}

type jobRequest struct {
	job          job
	imageRef     string
	imageLayout  string
	workspace    string
	goBuildCache string
	goModCache   string
	keep         bool
}

func runJob(ctx context.Context, runtime chamberRuntimeShared.Runtime, provisioner chamberBundleShared.Provisioner, request jobRequest) jobResult {
	containerID := "chamber-ci-" + request.job.name + "-" + uuid.NewString()
	result := jobResult{name: request.job.name, exitCode: 1}

	logging.Info(ctx, "CI job started", "job", request.job.name, "args", request.job.args)
	provisioned, err := provisioner.Provision(ctx, chamberBundleShared.ProvisionRequest{
		ContainerID: containerID,
		ImageLayout: request.imageLayout,
		ImageRef:    request.imageRef,
		Process: chamberBundleShared.ProcessSpec{
			Args: request.job.args,
			Env: []string{
				"PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"HOME=/tmp",
				"GOCACHE=/gocache",
				"GOMODCACHE=/gomodcache",
				"CGO_ENABLED=0",
			},
			Cwd: "/workspace",
		},
		Mounts: []chamberBundleShared.Mount{
			{Source: request.workspace, Target: "/workspace"},
			{Source: request.goBuildCache, Target: "/gocache"},
			{Source: request.goModCache, Target: "/gomodcache"},
		},
	})
	if err != nil {
		result.err = fmt.Errorf("provision job bundle: %w", err)
		return result
	}
	if !request.keep {
		defer func() {
			if err := os.RemoveAll(provisioned.BundlePath); err != nil && result.err == nil {
				result.err = fmt.Errorf("remove job bundle: %w", err)
			}
		}()
	}

	container, err := runtime.Run(ctx, chamberRuntimeShared.RunRequest{Bundle: provisioned})
	if err != nil {
		result.err = fmt.Errorf("run job container: %w", err)
		return result
	}
	waitResult, waitErr := container.Wait()
	result.exitCode = waitResult.ExitCode
	result.err = waitErr

	if stdout, err := container.ReadLog(chamberRuntimeShared.StdoutLogStream); err == nil {
		result.stdout = stdout
	} else if result.err == nil {
		result.err = fmt.Errorf("read stdout: %w", err)
	}
	if stderr, err := container.ReadLog(chamberRuntimeShared.StderrLogStream); err == nil {
		result.stderr = stderr
	} else if result.err == nil {
		result.err = fmt.Errorf("read stderr: %w", err)
	}

	if deleteErr := container.Delete(context.Background(), true); deleteErr != nil && result.err == nil && !looksAlreadyDeleted(deleteErr) {
		result.err = fmt.Errorf("delete runtime container: %w", deleteErr)
	}
	return result
}

func logResult(ctx context.Context, result jobResult) {
	if len(result.stdout) > 0 {
		logging.Info(ctx, "CI job output", "job", result.name, "stream", "stdout", "output", string(result.stdout))
	}
	if len(result.stderr) > 0 {
		logging.Info(ctx, "CI job output", "job", result.name, "stream", "stderr", "output", string(result.stderr))
	}
	if result.err != nil {
		logging.Error(ctx, "CI job failed", "job", result.name, "exit_code", result.exitCode, "error", result.err)
		return
	}
	if result.exitCode != 0 {
		logging.Error(ctx, "CI job failed", "job", result.name, "exit_code", result.exitCode)
		return
	}
	logging.Info(ctx, "CI job passed", "job", result.name)
}

func finalExitCode(results []jobResult) int {
	for _, result := range results {
		if result.err != nil {
			return 1
		}
		if result.exitCode != 0 {
			return result.exitCode
		}
	}
	return 0
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
