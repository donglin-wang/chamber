// Package lima provides Chamber's Lima-backed machine implementation.
// It shells out to limactl for local Linux VM lifecycle and command execution.
package lima

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	chamberMachine "github.com/donglin-wang/chamber/pkg/machine"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

const (
	providerName         = "lima"
	defaultLimactlPath   = "limactl"
	defaultStartTimeout  = 5 * time.Minute
	defaultSpecDirectory = "specs"
	defaultLimaHome      = "lima"
)

var _ chamberMachine.Machine = (*Machine)(nil)

type Machine struct {
	config           chamberMachine.Config
	limactlPath      string
	limaHome         string
	startTimeout     time.Duration
	runner           commandRunner
	directoryManager localfs.DirectoryManager
	descriptor       chamberMachine.Descriptor
}

type Option func(*Machine)

type commandRunner interface {
	Run(context.Context, commandInvocation) (commandResult, error)
}

type commandInvocation struct {
	Path   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type commandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

func WithLimactlPath(path string) Option {
	return func(machine *Machine) {
		if path != "" {
			machine.limactlPath = path
		}
	}
}

func WithLimaHome(path string) Option {
	return func(machine *Machine) {
		if path != "" {
			machine.limaHome = path
		}
	}
}

func WithStartTimeout(timeout time.Duration) Option {
	return func(machine *Machine) {
		if timeout > 0 {
			machine.startTimeout = timeout
		}
	}
}

func withCommandRunner(runner commandRunner) Option {
	return func(machine *Machine) {
		if runner != nil {
			machine.runner = runner
		}
	}
}

func New(ctx context.Context, config chamberMachine.Config, directoryManager localfs.DirectoryManager, options ...Option) (*Machine, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if directoryManager == nil {
		return nil, fmt.Errorf("directory manager is required")
	}
	if err := chamberLogging.Configure(config.Logging, nil); err != nil {
		return nil, err
	}
	resolved, err := chamberMachine.Resolve(config, chamberMachine.Override{})
	if err != nil {
		return nil, err
	}

	machine := &Machine{
		config:           resolved,
		limactlPath:      defaultLimactlPath,
		limaHome:         filepath.Join(resolved.Root, defaultLimaHome),
		startTimeout:     defaultStartTimeout,
		runner:           execCommandRunner{},
		directoryManager: directoryManager,
		descriptor:       descriptorFromConfig(resolved, chamberMachine.StatusUnknown, ""),
	}
	for _, option := range options {
		option(machine)
	}
	if machine.limaHome != "" {
		var err error
		machine.limaHome, err = filepath.Abs(machine.limaHome)
		if err != nil {
			return nil, fmt.Errorf("resolve Lima home: %w", err)
		}
	}

	if err := directoryManager.MkdirPrivate(resolved.Root); err != nil {
		return nil, fmt.Errorf("create machine root: %w", err)
	}
	if machine.limaHome != "" {
		if err := directoryManager.MkdirPrivate(machine.limaHome); err != nil {
			return nil, fmt.Errorf("create Lima home: %w", err)
		}
	}
	specPath, err := machine.writeSpec()
	if err != nil {
		return nil, err
	}

	descriptor, err := machine.inspect(ctx)
	if err != nil {
		return nil, err
	}
	if descriptor.Status == chamberMachine.StatusMissing {
		if err := machine.runLifecycle(ctx, "create", "--name", resolved.Name, specPath, "--tty=false"); err != nil {
			return nil, fmt.Errorf("create Lima machine %q: %w", resolved.Name, err)
		}
		descriptor = descriptorFromConfig(resolved, chamberMachine.StatusStopped, "")
	}
	if descriptor.Status == chamberMachine.StatusBroken {
		return nil, fmt.Errorf("Lima machine %q is broken", resolved.Name)
	}
	if resolved.Start && descriptor.Status != chamberMachine.StatusRunning {
		if err := machine.runLifecycle(ctx, "start", resolved.Name, "--timeout", machine.startTimeout.String(), "--tty=false"); err != nil {
			return nil, fmt.Errorf("start Lima machine %q: %w", resolved.Name, err)
		}
		descriptor.Status = chamberMachine.StatusRunning
	}
	machine.descriptor = descriptor

	return machine, nil
}

func (m *Machine) Descriptor() chamberMachine.Descriptor {
	if m == nil {
		return chamberMachine.Descriptor{}
	}
	return m.descriptor
}

func (m *Machine) Run(ctx context.Context, request chamberMachine.RunRequest) (chamberMachine.RunResult, error) {
	if m == nil {
		return chamberMachine.RunResult{}, fmt.Errorf("machine is required")
	}
	if len(request.Args) == 0 {
		return chamberMachine.RunResult{}, fmt.Errorf("machine run args are required")
	}

	args := []string{"shell"}
	if request.Workdir != "" {
		args = append(args, "--workdir", request.Workdir)
	}
	args = append(args, m.config.Name)
	if len(request.Env) > 0 {
		args = append(args, "env")
		args = append(args, request.Env...)
	}
	args = append(args, request.Args...)

	result, err := m.run(ctx, commandInvocation{
		Path:   m.limactlPath,
		Args:   args,
		Stdin:  request.Stdin,
		Stdout: request.Stdout,
		Stderr: request.Stderr,
	})
	if err != nil {
		return chamberMachine.RunResult{}, err
	}
	return chamberMachine.RunResult{ExitCode: result.ExitCode}, nil
}

func (m *Machine) Stop(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("machine is required")
	}
	if err := m.runLifecycle(ctx, "stop", m.config.Name); err != nil {
		return fmt.Errorf("stop Lima machine %q: %w", m.config.Name, err)
	}
	m.descriptor.Status = chamberMachine.StatusStopped
	return nil
}

func (m *Machine) Delete(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("machine is required")
	}
	if err := m.runLifecycle(ctx, "delete", "--force", m.config.Name, "--tty=false"); err != nil {
		return fmt.Errorf("delete Lima machine %q: %w", m.config.Name, err)
	}
	m.descriptor.Status = chamberMachine.StatusMissing
	return nil
}

func (m *Machine) writeSpec() (string, error) {
	specPath := filepath.Join(m.config.Root, defaultSpecDirectory, m.config.Name+".yaml")
	rendered, err := renderSpec(m.config.Spec)
	if err != nil {
		return "", err
	}
	if err := m.directoryManager.MkdirPrivateParent(specPath); err != nil {
		return "", fmt.Errorf("create Lima spec parent: %w", err)
	}

	tmp, err := m.directoryManager.CreateTemp(filepath.Dir(specPath), "."+filepath.Base(specPath)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temporary Lima spec: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(rendered); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write temporary Lima spec: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temporary Lima spec: %w", err)
	}
	if err := os.Rename(tmpPath, specPath); err != nil {
		return "", fmt.Errorf("commit Lima spec: %w", err)
	}
	committed = true
	return specPath, nil
}

func (m *Machine) inspect(ctx context.Context) (chamberMachine.Descriptor, error) {
	result, err := m.run(ctx, commandInvocation{
		Path: m.limactlPath,
		Args: []string{"list", "--format", "json", m.config.Name},
	})
	if err != nil {
		return chamberMachine.Descriptor{}, err
	}
	if result.ExitCode != 0 {
		if isMissingInstanceOutput(result.Stdout, result.Stderr) {
			return descriptorFromConfig(m.config, chamberMachine.StatusMissing, ""), nil
		}
		return chamberMachine.Descriptor{}, fmt.Errorf("inspect Lima machine %q failed with exit code %d: %s", m.config.Name, result.ExitCode, strings.TrimSpace(string(result.Stderr)))
	}
	descriptor, err := parseListOutput(result.Stdout, m.config)
	if err != nil {
		return chamberMachine.Descriptor{}, err
	}
	return descriptor, nil
}

func isMissingInstanceOutput(stdout []byte, stderr []byte) bool {
	output := strings.ToLower(string(stdout) + "\n" + string(stderr))
	return strings.Contains(output, "not found") ||
		strings.Contains(output, "no instance matching") ||
		strings.Contains(output, "unmatched instances")
}

func (m *Machine) runLifecycle(ctx context.Context, args ...string) error {
	result, err := m.run(ctx, commandInvocation{
		Path: m.limactlPath,
		Args: args,
	})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("limactl %s failed with exit code %d: %s", strings.Join(args, " "), result.ExitCode, strings.TrimSpace(string(result.Stderr)))
	}
	return nil
}

func (m *Machine) run(ctx context.Context, invocation commandInvocation) (commandResult, error) {
	if m.limaHome != "" {
		invocation.Env = append([]string{"LIMA_HOME=" + m.limaHome}, invocation.Env...)
	}
	return m.runner.Run(ctx, invocation)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, invocation commandInvocation) (commandResult, error) {
	cmd := exec.CommandContext(ctx, invocation.Path, invocation.Args...)
	if len(invocation.Env) > 0 {
		cmd.Env = append(os.Environ(), invocation.Env...)
	}
	cmd.Stdin = invocation.Stdin

	var stdout bytes.Buffer
	if invocation.Stdout != nil {
		cmd.Stdout = invocation.Stdout
	} else {
		cmd.Stdout = &stdout
	}
	var stderr bytes.Buffer
	if invocation.Stderr != nil {
		cmd.Stderr = invocation.Stderr
	} else {
		cmd.Stderr = &stderr
	}

	err := cmd.Run()
	result := commandResult{
		ExitCode: exitCode(err),
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
	}
	if err != nil && result.ExitCode == 0 {
		return result, fmt.Errorf("run %s: %w", invocation.Path, err)
	}
	return result, nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 0
}

type limaListInstance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Dir    string `json:"dir"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	CPUs   int    `json:"cpus"`
	Memory any    `json:"memory"`
	Disk   any    `json:"disk"`
}

func parseListOutput(data []byte, config chamberMachine.Config) (chamberMachine.Descriptor, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) || bytes.Equal(data, []byte("[]")) {
		return descriptorFromConfig(config, chamberMachine.StatusMissing, ""), nil
	}

	var instance limaListInstance
	if err := json.Unmarshal(data, &instance); err == nil && instance.Name != "" {
		return descriptorFromInstance(instance, config), nil
	}

	var instances []limaListInstance
	if err := json.Unmarshal(data, &instances); err != nil {
		return chamberMachine.Descriptor{}, fmt.Errorf("decode Lima list output: %w", err)
	}
	if len(instances) == 0 {
		return descriptorFromConfig(config, chamberMachine.StatusMissing, ""), nil
	}
	return descriptorFromInstance(instances[0], config), nil
}

func descriptorFromInstance(instance limaListInstance, config chamberMachine.Config) chamberMachine.Descriptor {
	descriptor := descriptorFromConfig(config, mapStatus(instance.Status), instance.Dir)
	if instance.Name != "" {
		descriptor.Name = instance.Name
	}
	if instance.OS != "" {
		descriptor.OS = strings.ToLower(instance.OS)
	}
	if instance.Arch != "" {
		descriptor.Arch = normalizeLimaArch(instance.Arch)
	}
	if instance.CPUs != 0 {
		descriptor.CPUs = instance.CPUs
	}
	if memory := numericBytes(instance.Memory); memory != 0 {
		descriptor.MemoryBytes = memory
	}
	if disk := numericBytes(instance.Disk); disk != 0 {
		descriptor.DiskBytes = disk
	}
	return descriptor
}

func descriptorFromConfig(config chamberMachine.Config, status chamberMachine.Status, directory string) chamberMachine.Descriptor {
	return chamberMachine.Descriptor{
		Name:        config.Name,
		Status:      status,
		Provider:    providerName,
		Directory:   directory,
		OS:          config.Spec.OS,
		Arch:        config.Spec.Arch,
		CPUs:        config.Spec.CPUs,
		MemoryBytes: config.Spec.MemoryBytes,
		DiskBytes:   config.Spec.DiskBytes,
	}
}

func mapStatus(status string) chamberMachine.Status {
	switch strings.ToLower(status) {
	case "":
		return chamberMachine.StatusUnknown
	case "running":
		return chamberMachine.StatusRunning
	case "stopped":
		return chamberMachine.StatusStopped
	case "broken":
		return chamberMachine.StatusBroken
	default:
		return chamberMachine.StatusUnknown
	}
}

func normalizeLimaArch(arch string) string {
	switch arch {
	case "aarch64":
		return "arm64"
	case "x86_64":
		return "amd64"
	default:
		return arch
	}
}

func numericBytes(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}
