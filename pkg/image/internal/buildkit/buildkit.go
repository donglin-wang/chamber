package buildkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

const (
	defaultSnapshotter = "native"
	readinessTimeout   = 20 * time.Second
	readinessInterval  = 100 * time.Millisecond
	shutdownTimeout    = 5 * time.Second

	buildKitStateTempPattern  = ".bk-state-*"
	buildKitRunTempPattern    = ".bk-run-*"
	buildKitHomeTempPattern   = ".bk-home-*"
	buildKitTmpTempPattern    = ".bk-tmp-*"
	buildKitOutputTempPattern = ".bk-output-*.tar"

	linuxPathnameSocketMaxBytes   = 107
	maxMkdirTempRandomSuffixBytes = 10
)

type Builder struct {
	config      chamberImage.BuildKitConfig
	root        string
	paths       toolPaths
	snapshotter string
	client      *http.Client
	workspace   *hostfs.Workspace
	logger      *chamberLogging.SlogLogger
}

// New returns a builder with BuildKit, RootlessKit, and runc installed or
// verified below the image root.
func New(ctx context.Context, config chamberImage.BuildKitConfig, workspace *hostfs.Workspace, logger *chamberLogging.SlogLogger) (Builder, error) {
	builder := Builder{
		config:    config,
		client:    http.DefaultClient,
		workspace: workspace,
		logger:    logger,
	}
	if workspace != nil {
		builder.root = workspace.Root()
	}
	if err := builder.install(ctx); err != nil {
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
	if err := preflightRootlessBuildHost(); err != nil {
		return err
	}
	dirs, cleanup, err := b.createBuildDirectories(outputLayout)
	if err != nil {
		return err
	}
	defer cleanup()

	daemon, err := b.startBuildkitd(ctx, dirs)
	if err != nil {
		return err
	}
	defer daemon.stop()

	if err := b.waitForReadiness(ctx, daemon, dirs); err != nil {
		return err
	}
	if err := b.runBuildctl(ctx, build, dirs); err != nil {
		return err
	}
	if err := b.extractOCITar(dirs.outputArchive, outputLayout); err != nil {
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

func validateBuildContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: image build canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	return nil
}

func (b *Builder) install(ctx context.Context) error {
	if err := b.validate(ctx); err != nil {
		return err
	}
	paths, err := b.installTools(ctx)
	if err != nil {
		return err
	}
	b.paths = paths
	b.snapshotter = strings.TrimSpace(b.config.Snapshotter)
	if b.snapshotter == "" {
		b.snapshotter = defaultSnapshotter
	}
	return nil
}

func (b Builder) validate(ctx context.Context) error {
	if err := validateBuildContext(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(b.root) == "" {
		return fmt.Errorf("%w: BuildKit root is required", chamberErrors.ErrInvalidRequest)
	}
	if !filepath.IsAbs(b.root) {
		return fmt.Errorf("%w: BuildKit root must be absolute", chamberErrors.ErrInvalidRequest)
	}
	if b.workspace == nil {
		return fmt.Errorf("%w: image workspace is required", chamberErrors.ErrInvalidRequest)
	}
	return validateBuildKitSocketRoot(b.workspace.TmpRoot())
}

type buildDirectories struct {
	stateDir      string
	runDir        string
	homeDir       string
	tmpDir        string
	dockerConfig  string
	outputArchive string
}

func (b Builder) createBuildDirectories(outputLayout string) (buildDirectories, func(), error) {
	tmpRoot := filepath.Dir(outputLayout)
	tmpRel, err := filepath.Rel(b.workspace.TmpRoot(), tmpRoot)
	if err != nil || tmpRel == ".." || strings.HasPrefix(tmpRel, ".."+string(filepath.Separator)) {
		return buildDirectories{}, nil, fmt.Errorf("%w: BuildKit output layout must be below image temporary root", chamberErrors.ErrInvalidRequest)
	}
	var dirs buildDirectories
	var created []string
	for name, target := range map[string]*string{
		buildKitStateTempPattern: &dirs.stateDir,
		buildKitRunTempPattern:   &dirs.runDir,
		buildKitHomeTempPattern:  &dirs.homeDir,
		buildKitTmpTempPattern:   &dirs.tmpDir,
	} {
		path, err := b.workspace.MkdirTemp(tmpRel, name)
		if err != nil {
			for _, cleanupPath := range created {
				_ = os.RemoveAll(cleanupPath)
			}
			return buildDirectories{}, nil, fmt.Errorf("%w: create BuildKit temporary directory: %w", chamberErrors.ErrFilesystemFailed, err)
		}
		*target = path
		created = append(created, path)
	}
	if err := validateBuildKitSocketPaths(dirs.runDir); err != nil {
		for _, cleanupPath := range created {
			_ = os.RemoveAll(cleanupPath)
		}
		return buildDirectories{}, nil, err
	}

	dirs.dockerConfig = filepath.Join(dirs.homeDir, ".docker")
	if err := os.MkdirAll(dirs.dockerConfig, 0700); err != nil {
		for _, cleanupPath := range created {
			_ = os.RemoveAll(cleanupPath)
		}
		return buildDirectories{}, nil, fmt.Errorf("%w: create BuildKit Docker config directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	archive, err := b.workspace.CreateTemp(tmpRel, buildKitOutputTempPattern)
	if err != nil {
		for _, cleanupPath := range created {
			_ = os.RemoveAll(cleanupPath)
		}
		return buildDirectories{}, nil, fmt.Errorf("%w: create temporary OCI archive: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	dirs.outputArchive = archive.Name()
	if err := archive.Close(); err != nil {
		_ = os.Remove(dirs.outputArchive)
		for _, cleanupPath := range created {
			_ = os.RemoveAll(cleanupPath)
		}
		return buildDirectories{}, nil, fmt.Errorf("%w: close temporary OCI archive: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	cleanup := func() {
		_ = os.Remove(dirs.outputArchive)
		for _, path := range created {
			_ = os.RemoveAll(path)
		}
	}
	return dirs, cleanup, nil
}

func validateBuildKitSocketRoot(tmpRoot string) error {
	runDir := filepath.Join(tmpRoot, buildKitRunDirectoryWithLongestSuffix())
	return validateBuildKitSocketPaths(runDir)
}

func buildKitRunDirectoryWithLongestSuffix() string {
	prefix, suffix, _ := strings.Cut(buildKitRunTempPattern, "*")
	return prefix + strings.Repeat("0", maxMkdirTempRandomSuffixBytes) + suffix
}

func validateBuildKitSocketPaths(runDir string) error {
	for label, path := range map[string]string{
		"BuildKit daemon": filepath.Join(runDir, "buildkitd.sock"),
		"RootlessKit API": filepath.Join(runDir, "api.sock"),
	} {
		if len(path) > linuxPathnameSocketMaxBytes {
			return fmt.Errorf("%w: %s Unix socket path is %d bytes, exceeds Linux pathname socket limit of %d bytes; choose a shorter image root or temporary root", chamberErrors.ErrInvalidRequest, label, len(path), linuxPathnameSocketMaxBytes)
		}
	}
	return nil
}

func (b Builder) startBuildkitd(ctx context.Context, dirs buildDirectories) (*daemonProcess, error) {
	command, err := b.buildkitdCommand(ctx, dirs)
	if err != nil {
		return nil, err
	}
	output := &lockedBuffer{}
	stdout := newProcessOutputBuffer(ctx, b.logger, "buildkitd", "stdout", output)
	stderr := newProcessOutputBuffer(ctx, b.logger, "buildkitd", "stderr", output)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: image build canceled while starting BuildKit daemon: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return nil, classifyBuildError(chamberErrors.ErrBuildFailed, "start BuildKit daemon", err, output.String())
	}
	daemon := &daemonProcess{
		cmd:     command,
		output:  output,
		writers: []*processOutputBuffer{stdout, stderr},
		done:    make(chan error, 1),
	}
	go func() {
		err := command.Wait()
		for _, writer := range daemon.writers {
			writer.Flush()
		}
		daemon.done <- err
	}()
	return daemon, nil
}

func (b Builder) waitForReadiness(ctx context.Context, daemon *daemonProcess, dirs buildDirectories) error {
	deadline := time.NewTimer(readinessTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()

	for {
		if err := b.runBuildctlDebugWorkers(ctx, dirs); err == nil {
			return nil
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: image build canceled while waiting for BuildKit daemon: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		select {
		case err := <-daemon.done:
			return classifyBuildError(chamberErrors.ErrBuildFailed, "BuildKit daemon exited before readiness", err, daemon.output.String())
		case <-ctx.Done():
			return fmt.Errorf("%w: image build canceled while waiting for BuildKit daemon: %w", chamberErrors.ErrCanceled, ctx.Err())
		case <-deadline.C:
			return classifyBuildError(chamberErrors.ErrBuildFailed, "BuildKit daemon readiness timed out", context.DeadlineExceeded, daemon.output.String())
		case <-ticker.C:
		}
	}
}

func (b Builder) runBuildctlDebugWorkers(ctx context.Context, dirs buildDirectories) error {
	command := exec.CommandContext(ctx, b.paths.buildctl, "--addr", buildkitAddress(dirs), "debug", "workers")
	command.Env = b.buildEnvironment(dirs)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildctl debug workers failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (b Builder) runBuildctl(ctx context.Context, build resolvedRequest, dirs buildDirectories) error {
	command, err := b.buildctlBuildCommand(ctx, build, dirs)
	if err != nil {
		return err
	}
	output := &lockedBuffer{}
	stdout := newProcessOutputBuffer(ctx, b.logger, "buildctl", "stdout", output)
	stderr := newProcessOutputBuffer(ctx, b.logger, "buildctl", "stderr", output)
	command.Stdout = stdout
	command.Stderr = stderr
	err = command.Run()
	stdout.Flush()
	stderr.Flush()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: image build canceled while running buildctl: %w", chamberErrors.ErrCanceled, ctxErr)
		}
		return classifyBuildError(chamberErrors.ErrBuildFailed, "run buildctl build", err, output.String())
	}
	return nil
}

func (b Builder) buildkitdCommand(ctx context.Context, dirs buildDirectories) (*exec.Cmd, error) {
	if err := b.requirePrepared(); err != nil {
		return nil, err
	}
	args := []string{
		"--state-dir", dirs.runDir,
		"--net=host",
		b.paths.buildkitd,
		"--addr", buildkitAddress(dirs),
		"--root", dirs.stateDir,
		"--oci-worker=true",
		"--containerd-worker=false",
		"--oci-worker-binary", b.paths.runc,
		"--oci-worker-snapshotter", b.snapshotter,
	}
	command := exec.CommandContext(ctx, b.paths.rootlesskit, args...)
	command.Env = b.buildEnvironment(dirs)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return command.Process.Signal(syscall.SIGTERM)
	}
	command.WaitDelay = shutdownTimeout
	return command, nil
}

func (b Builder) buildctlBuildCommand(ctx context.Context, build resolvedRequest, dirs buildDirectories) (*exec.Cmd, error) {
	if err := b.requirePrepared(); err != nil {
		return nil, err
	}
	args := []string{
		"--addr", buildkitAddress(dirs),
		"build",
		"--progress", "plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + build.contextPath,
		"--local", "dockerfile=" + build.contextPath,
		"--opt", "filename=" + build.dockerfileRelPath,
		"--opt", "platform=" + platformString(build.platform),
		"--output", "type=oci,dest=" + dirs.outputArchive,
	}
	if build.target != "" {
		args = append(args, "--opt", "target="+build.target)
	}
	keys := make([]string, 0, len(build.buildArgs))
	for key := range build.buildArgs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--opt", "build-arg:"+key+"="+build.buildArgs[key])
	}
	command := exec.CommandContext(ctx, b.paths.buildctl, args...)
	command.Dir = build.contextPath
	command.Env = b.buildEnvironment(dirs)
	return command, nil
}

func (b Builder) buildEnvironment(dirs buildDirectories) []string {
	env := os.Environ()
	env = setEnv(env, "XDG_RUNTIME_DIR", dirs.runDir)
	env = setEnv(env, "TMPDIR", dirs.tmpDir)
	env = setEnv(env, "HOME", dirs.homeDir)
	env = setEnv(env, "DOCKER_CONFIG", dirs.dockerConfig)
	env = setEnv(env, "PATH", filepath.Join(b.root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	return env
}

func (b Builder) requirePrepared() error {
	if strings.TrimSpace(b.paths.buildctl) == "" || strings.TrimSpace(b.paths.buildkitd) == "" || strings.TrimSpace(b.paths.rootlesskit) == "" || strings.TrimSpace(b.paths.runc) == "" || strings.TrimSpace(b.snapshotter) == "" {
		return fmt.Errorf("%w: BuildKit builder is not prepared", chamberErrors.ErrInvalidRequest)
	}
	return nil
}

func buildkitAddress(dirs buildDirectories) string {
	return "unix://" + filepath.Join(dirs.runDir, "buildkitd.sock")
}

func platformString(platform chamberImage.Platform) string {
	platform = chamberImage.NormalizePlatform(platform)
	value := platform.OS + "/" + platform.Architecture
	if platform.Variant != "" {
		value += "/" + platform.Variant
	}
	return value
}

func setEnv(env []string, key string, value string) []string {
	prefix := key + "="
	for i, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func classifyBuildError(code error, action string, err error, output string) error {
	message := trimOutput(output)
	if classifyUnsupportedHostMessage(message) {
		if err != nil && message != "" {
			return fmt.Errorf("%w: %s: %w: %s", chamberErrors.ErrUnsupportedHost, action, err, message)
		}
		if message != "" {
			return fmt.Errorf("%w: %s: %s", chamberErrors.ErrUnsupportedHost, action, message)
		}
		return fmt.Errorf("%w: %s", chamberErrors.ErrUnsupportedHost, action)
	}
	if err != nil && message != "" {
		return fmt.Errorf("%w: %s: %w: %s", code, action, err, message)
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %w", code, action, err)
	}
	if message != "" {
		return fmt.Errorf("%w: %s: %s", code, action, message)
	}
	return fmt.Errorf("%w: %s", code, action)
}

func trimOutput(output string) string {
	output = strings.TrimSpace(output)
	const max = 4096
	if len(output) <= max {
		return output
	}
	return output[:max] + "...(truncated)"
}

type daemonProcess struct {
	cmd     *exec.Cmd
	output  *lockedBuffer
	writers []*processOutputBuffer
	done    chan error
}

func (d *daemonProcess) stop() {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return
	}
	if d.cmd.ProcessState != nil {
		return
	}
	for _, writer := range d.writers {
		writer.SuppressLogs()
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
		return
	case <-time.After(shutdownTimeout):
		_ = syscall.Kill(-d.cmd.Process.Pid, syscall.SIGKILL)
		<-d.done
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type processOutputBuffer struct {
	captured *lockedBuffer
	logs     *processLogWriter
}

func newProcessOutputBuffer(ctx context.Context, logger *chamberLogging.SlogLogger, process string, stream string, captured *lockedBuffer) *processOutputBuffer {
	return &processOutputBuffer{
		captured: captured,
		logs: &processLogWriter{
			ctx:     ctx,
			logger:  logger,
			process: process,
			stream:  stream,
		},
	}
}

func (b *processOutputBuffer) Write(p []byte) (int, error) {
	n, err := b.captured.Write(p)
	if n > 0 {
		_, _ = b.logs.Write(p[:n])
	}
	return n, err
}

func (b *processOutputBuffer) Flush() {
	b.logs.Flush()
}

func (b *processOutputBuffer) SuppressLogs() {
	b.logs.Suppress()
}

type processLogWriter struct {
	ctx      context.Context
	logger   *chamberLogging.SlogLogger
	process  string
	stream   string
	mu       sync.Mutex
	line     []byte
	suppress bool
}

func (w *processLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	written := len(p)
	for len(p) > 0 {
		index := bytes.IndexByte(p, '\n')
		if index < 0 {
			w.line = append(w.line, p...)
			break
		}
		w.line = append(w.line, p[:index]...)
		w.emitLocked()
		p = p[index+1:]
	}
	return written, nil
}

func (w *processLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emitLocked()
}

func (w *processLogWriter) Suppress() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.suppress = true
	w.line = w.line[:0]
}

func (w *processLogWriter) emitLocked() {
	line := strings.TrimRight(string(w.line), "\r")
	w.line = w.line[:0]
	if w.suppress {
		return
	}
	if strings.TrimSpace(line) == "" {
		return
	}
	chamberLogging.InfoWith(w.logger, w.ctx, "buildkit process output",
		"process", w.process,
		"stream", w.stream,
		"line", line,
	)
}
