package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberDirectoryProvisioner "github.com/donglin-wang/chamber/pkg/bundle/directory"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImagePuller "github.com/donglin-wang/chamber/pkg/image/puller"
	chamberMachine "github.com/donglin-wang/chamber/pkg/machine"
	chamberLimaMachine "github.com/donglin-wang/chamber/pkg/machine/lima"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuncRuntime "github.com/donglin-wang/chamber/pkg/runtime/runc"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	"github.com/donglin-wang/chamber/pkg/shared/logging"
	"github.com/google/uuid"
)

const (
	machineModeAuto     = "auto"
	machineModeNone     = "none"
	machineProviderLima = "lima"
)

type config struct {
	root            string
	workdir         string
	image           string
	timeout         time.Duration
	keep            bool
	machine         string
	machineProvider string
	machineRoot     string
	machineKeep     bool
	exitCode        int
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
	flag.StringVar(&cfg.machine, "machine", machineModeAuto, "machine mode: auto, none, or a Lima machine name")
	flag.StringVar(&cfg.machineProvider, "machine-provider", machineProviderLima, "machine provider used when -machine is not none")
	flag.StringVar(&cfg.machineRoot, "machine-root", "", "root directory for Chamber machine state; defaults outside the workspace")
	flag.BoolVar(&cfg.machineKeep, "machine-keep", true, "keep the machine running after host-side CI finishes")
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
	if useMachine(cfg.machine) {
		return runInMachine(ctx, cfg, workspace, loggingConfig)
	}

	return runLocal(ctx, cfg, workspace, loggingConfig)
}

func useMachine(mode string) bool {
	switch strings.TrimSpace(mode) {
	case "", machineModeAuto:
		return goruntime.GOOS != "linux"
	case machineModeNone:
		return false
	default:
		return true
	}
}

func runLocal(ctx context.Context, cfg *config, workspace string, loggingConfig logging.Config) error {
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

	runtime, err := chamberRuncRuntime.New(ctx, chamberRuntime.Config{
		RuntimeRoot:   paths.runtimeRoot,
		RuntimeBinDir: paths.runtimeBinDir,
		Logging:       loggingConfig,
	}, directoryManager)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	binary := runtime.Binary()
	descriptor := runtime.Descriptor()
	logging.Info(ctx, "CI runtime ready", "runtime", descriptor.Name, "version", descriptor.Version, "path", binary.Path)

	puller, err := chamberImagePuller.New(chamberImage.Config{
		Root:    paths.imageRoot,
		Logging: loggingConfig,
	}, directoryManager)
	if err != nil {
		return fmt.Errorf("create image puller: %w", err)
	}
	imageLayout, err := ensureImage(ctx, puller, paths.imageRoot, cfg.image)
	if err != nil {
		return err
	}

	provisioner, err := chamberDirectoryProvisioner.New(
		chamberBundle.Config{
			Root:    paths.bundleRoot,
			Logging: loggingConfig,
		},
		directoryManager,
		chamberDirectoryProvisioner.WithIDMap(uint32(os.Geteuid()), uint32(os.Getegid())),
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

func runInMachine(ctx context.Context, cfg *config, workspace string, loggingConfig logging.Config) error {
	if cfg.machineProvider != machineProviderLima {
		return fmt.Errorf("unsupported machine provider %q", cfg.machineProvider)
	}

	machineRoot, err := resolveMachineRoot(cfg.machineRoot, workspace)
	if err != nil {
		return fmt.Errorf("resolve machine root: %w", err)
	}
	directoryManager := localfs.NewDirectoryManager()
	if err := directoryManager.MkdirPrivate(machineRoot); err != nil {
		return fmt.Errorf("create machine root: %w", err)
	}

	guestRoot := filepath.Join(machineRoot, "guest")
	if err := directoryManager.MkdirPrivate(guestRoot); err != nil {
		return fmt.Errorf("create machine guest root: %w", err)
	}
	runnerPath, err := buildGuestRunner(ctx, workspace, guestRoot)
	if err != nil {
		return err
	}

	ciRoot, err := explicitCIRoot(cfg.root, workspace)
	if err != nil {
		return err
	}
	spec, err := ciMachineSpec(workspace, guestRoot, ciRoot)
	if err != nil {
		return err
	}
	machineName := cfg.machine
	if strings.TrimSpace(machineName) == "" || machineName == machineModeAuto {
		machineName = defaultMachineName(workspace)
	}

	vm, err := chamberLimaMachine.New(ctx, chamberMachine.Config{
		Root:    machineRoot,
		Name:    machineName,
		Spec:    spec,
		Start:   true,
		Logging: loggingConfig,
	}, directoryManager)
	if err != nil {
		return fmt.Errorf("create machine: %w", err)
	}
	descriptor := vm.Descriptor()
	logging.Info(ctx, "CI machine ready",
		"machine", descriptor.Name,
		"provider", descriptor.Provider,
		"status", descriptor.Status,
		"arch", descriptor.Arch,
	)

	args := guestCIArgs(runnerPath, cfg, workspace, ciRoot)
	result, err := vm.Run(ctx, chamberMachine.RunRequest{
		Args:    args,
		Workdir: workspace,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("run CI in machine: %w", err)
	}
	cfg.exitCode = result.ExitCode

	if !cfg.machineKeep {
		if err := vm.Stop(context.Background()); err != nil {
			return err
		}
	}
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

func resolveMachineRoot(root string, workspace string) (string, error) {
	var resolved string
	var err error
	if strings.TrimSpace(root) == "" {
		cacheRoot, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache directory: %w", err)
		}
		resolved = filepath.Join(cacheRoot, "chamber", "machines")
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
		return "", fmt.Errorf("machine root %q must be outside workspace %q", resolved, workspace)
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

func buildGuestRunner(ctx context.Context, workspace string, guestRoot string) (string, error) {
	binDir := filepath.Join(guestRoot, "bin")
	directoryManager := localfs.NewDirectoryManager()
	if err := directoryManager.MkdirPrivate(binDir); err != nil {
		return "", fmt.Errorf("create guest runner bin dir: %w", err)
	}

	goarch := goruntime.GOARCH
	runnerPath := filepath.Join(binDir, "chamber-ci-linux-"+goarch)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", runnerPath, "./cmd/ci")
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	logging.Info(ctx, "building Linux CI runner", "path", runnerPath, "goarch", goarch)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build Linux CI runner: %w", err)
	}
	return runnerPath, nil
}

func explicitCIRoot(root string, workspace string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	resolved, err := resolveRoot(root, workspace)
	if err != nil {
		return "", fmt.Errorf("resolve explicit CI root: %w", err)
	}
	return resolved, nil
}

func ciMachineSpec(workspace string, guestRoot string, ciRoot string) (chamberMachine.Spec, error) {
	mounts := []chamberMachine.Mount{}
	var err error
	mounts, err = appendMachineMount(mounts, workspace)
	if err != nil {
		return chamberMachine.Spec{}, err
	}
	mounts, err = appendMachineMount(mounts, guestRoot)
	if err != nil {
		return chamberMachine.Spec{}, err
	}
	if ciRoot != "" {
		mounts, err = appendMachineMount(mounts, ciRoot)
		if err != nil {
			return chamberMachine.Spec{}, err
		}
	}

	return chamberMachine.Spec{
		OS:          "linux",
		Arch:        goruntime.GOARCH,
		CPUs:        4,
		MemoryBytes: 4 * 1024 * 1024 * 1024,
		DiskBytes:   100 * 1024 * 1024 * 1024,
		Mounts:      mounts,
		SetupScript: ciMachineSetupScript,
	}, nil
}

func appendMachineMount(mounts []chamberMachine.Mount, source string) ([]chamberMachine.Mount, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, fmt.Errorf("resolve machine mount: %w", err)
	}
	for _, mount := range mounts {
		if pathContains(mount.Source, source) {
			return mounts, nil
		}
		if pathContains(source, mount.Source) {
			return nil, fmt.Errorf("machine mount %q overlaps existing mount %q", source, mount.Source)
		}
	}
	return append(mounts, chamberMachine.Mount{
		Source:   source,
		Target:   source,
		Writable: true,
	}), nil
}

func guestCIArgs(runnerPath string, cfg *config, workspace string, ciRoot string) []string {
	args := []string{
		runnerPath,
		"-machine=" + machineModeNone,
		"-workdir", workspace,
		"-image", cfg.image,
		"-timeout", cfg.timeout.String(),
	}
	if cfg.keep {
		args = append(args, "-keep=true")
	}
	if ciRoot != "" {
		args = append(args, "-root", ciRoot)
	}
	return args
}

func defaultMachineName(workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	return "cci-" + hex.EncodeToString(sum[:])[:8]
}

const ciMachineSetupScript = `#!/bin/bash
set -eux

sysctl -w kernel.unprivileged_userns_clone=1 || true
sysctl -w user.max_user_namespaces=28633 || true

if [ -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
  sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
fi
`

func ensureImage(ctx context.Context, puller chamberImage.Puller, imageRoot string, imageRef string) (string, error) {
	destination, err := chamberImage.Destination(imageRoot, imageRef)
	if err != nil {
		return "", fmt.Errorf("resolve image destination: %w", err)
	}
	if chamberImage.LayoutExists(destination) {
		logging.Info(ctx, "CI image reused", "image_ref", imageRef, "image_layout", destination)
		return destination, nil
	}

	logging.Info(ctx, "CI image pull started", "image_ref", imageRef)
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

func runJob(ctx context.Context, runtime chamberRuntime.Runtime, provisioner chamberBundle.Provisioner, request jobRequest) jobResult {
	containerID := "chamber-ci-" + request.job.name + "-" + uuid.NewString()
	result := jobResult{name: request.job.name, exitCode: 1}

	logging.Info(ctx, "CI job started", "job", request.job.name, "args", request.job.args)
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
