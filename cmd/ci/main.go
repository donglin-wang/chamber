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
	chamberRootlessProvisioner "github.com/donglin-wang/chamber/pkg/bundle/rootless"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImagePuller "github.com/donglin-wang/chamber/pkg/image/puller"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuncRuntime "github.com/donglin-wang/chamber/pkg/runtime/runc"
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
		fmt.Fprintf(os.Stderr, "chamber-ci: %v\n", err)
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
	if err := directoryManager.EnsurePrivateDir(root); err != nil {
		return fmt.Errorf("prepare CI root: %w", err)
	}
	fmt.Printf("root: %s\n", root)

	paths := ciPaths(root)
	for _, path := range []string{
		paths.imageRoot,
		paths.bundleRoot,
		paths.runtimeRoot,
		paths.runtimeBinDir,
		paths.goBuildCache,
		paths.goModCache,
	} {
		if err := directoryManager.EnsurePrivateDir(path); err != nil {
			return fmt.Errorf("prepare CI path %q: %w", path, err)
		}
	}

	runtime := chamberRuncRuntime.New(chamberRuntime.Config{
		RuntimeRoot:   paths.runtimeRoot,
		RuntimeBinDir: paths.runtimeBinDir,
		Name:          chamberRuntime.DefaultName,
		Logging:       loggingConfig,
	}, directoryManager)
	if binary, err := runtime.Ensure(ctx); err != nil {
		return fmt.Errorf("ensure runtime: %w", err)
	} else {
		fmt.Printf("runtime: %s %s at %s\n", binary.Name, binary.Version, binary.Path)
	}

	puller := chamberImagePuller.New(chamberImage.Config{
		Root:    paths.imageRoot,
		Logging: loggingConfig,
	}, directoryManager)
	imageLayout, err := ensureImage(ctx, puller, paths.imageRoot, cfg.image)
	if err != nil {
		return err
	}

	provisioner := chamberRootlessProvisioner.Provisioner{
		Config: chamberBundle.Config{
			Root:    paths.bundleRoot,
			Logging: loggingConfig,
		},
		UID:              uint32(os.Geteuid()),
		GID:              uint32(os.Getegid()),
		DirectoryManager: directoryManager,
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
		printResult(result)
	}

	cfg.exitCode = finalExitCode(results)
	if cfg.exitCode != 0 {
		return nil
	}
	fmt.Println("chamber-ci: all jobs passed")
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

func ensureImage(ctx context.Context, puller chamberImage.Puller, imageRoot string, imageRef string) (string, error) {
	destination, err := chamberImage.Destination(imageRoot, imageRef)
	if err != nil {
		return "", fmt.Errorf("resolve image destination: %w", err)
	}
	if chamberImage.LayoutExists(destination) {
		fmt.Printf("image: reusing %s at %s\n", imageRef, destination)
		return destination, nil
	}

	fmt.Printf("image: pulling %s\n", imageRef)
	pulled, err := puller.Pull(ctx, chamberImage.PullRequest{
		Reference:   imageRef,
		Destination: destination,
		Platform: chamberImage.Platform{
			OS: "linux",
		},
	})
	if err != nil {
		return "", fmt.Errorf("pull image %q: %w", imageRef, err)
	}
	fmt.Printf("image: pulled %s digest=%s bytes=%d\n", pulled.Reference, pulled.Digest, pulled.SizeBytes)
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

func runJob(ctx context.Context, runtime chamberRuntime.Runtime, provisioner chamberBundle.Provisioner, request jobRequest) jobResult {
	containerID := "chamber-ci-" + request.job.name + "-" + uuid.NewString()
	result := jobResult{name: request.job.name, exitCode: 1}

	fmt.Printf("\n==> %s: %v\n", request.job.name, request.job.args)
	provisioned, err := provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
		ContainerID: containerID,
		ImageLayout: request.imageLayout,
		ImageRef:    request.imageRef,
		Process: chamberBundle.ProcessSpec{
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
		Mounts: []chamberBundle.Mount{
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

	process, err := runtime.Run(ctx, chamberRuntime.RunRequest{Bundle: provisioned})
	if err != nil {
		result.err = fmt.Errorf("run job container: %w", err)
		return result
	}
	result.exitCode, result.err = process.Wait()

	if stdout, err := runtime.ReadLog(containerID, chamberRuntime.StdoutLogStream); err == nil {
		result.stdout = stdout
	} else if result.err == nil {
		result.err = fmt.Errorf("read stdout: %w", err)
	}
	if stderr, err := runtime.ReadLog(containerID, chamberRuntime.StderrLogStream); err == nil {
		result.stderr = stderr
	} else if result.err == nil {
		result.err = fmt.Errorf("read stderr: %w", err)
	}

	if deleteErr := runtime.Delete(context.Background(), chamberRuntime.DeleteRequest{
		ContainerID: containerID,
		Force:       true,
	}); deleteErr != nil && result.err == nil && !looksAlreadyDeleted(deleteErr) {
		result.err = fmt.Errorf("delete runtime container: %w", deleteErr)
	}
	return result
}

func printResult(result jobResult) {
	if len(result.stdout) > 0 {
		fmt.Printf("--- %s stdout ---\n%s", result.name, result.stdout)
		if result.stdout[len(result.stdout)-1] != '\n' {
			fmt.Println()
		}
	}
	if len(result.stderr) > 0 {
		fmt.Printf("--- %s stderr ---\n%s", result.name, result.stderr)
		if result.stderr[len(result.stderr)-1] != '\n' {
			fmt.Println()
		}
	}
	if result.err != nil {
		fmt.Printf("<== %s failed: exit=%d error=%v\n", result.name, result.exitCode, result.err)
		return
	}
	if result.exitCode != 0 {
		fmt.Printf("<== %s failed: exit=%d\n", result.name, result.exitCode)
		return
	}
	fmt.Printf("<== %s passed\n", result.name)
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
