package runtime

import (
	"context"
	"fmt"
	"io"
	goruntime "runtime"
	"sort"
	"strings"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

type Binary struct {
	Name string
	Path string
}

type Isolation string

const (
	ProcessIsolation Isolation = "process"
	VMIsolation      Isolation = "vm"
)

type Capabilities struct {
	Privileges []capability.Privilege
	Isolation  []Isolation
}

const RuntimeNameRunc = "runc"

type constructor func(context.Context, Config, localfs.DirectoryManager) (Runtime, error)

type implementationSpec struct {
	Capabilities Capabilities
	New          constructor
}

var implementations = map[string]implementationSpec{
	RuntimeNameRunc: {
		Capabilities: Capabilities{
			Privileges: []capability.Privilege{
				capability.Rootless,
			},
			Isolation: []Isolation{
				ProcessIsolation,
			},
		},
	},
}

type Descriptor struct {
	Name         string
	Version      string
	Capabilities Capabilities
}

type ContainerStatus string

const (
	ContainerStatusCreating ContainerStatus = "creating"
	ContainerStatusCreated  ContainerStatus = "created"
	ContainerStatusRunning  ContainerStatus = "running"
	ContainerStatusStopped  ContainerStatus = "stopped"
)

type Signal string

const (
	SignalTERM Signal = "TERM"
	SignalKILL Signal = "KILL"
	SignalINT  Signal = "INT"
)

func IsSupportedSignal(signal Signal) bool {
	switch signal {
	case SignalTERM, SignalKILL, SignalINT:
		return true
	default:
		return false
	}
}

type LogStream string

const (
	StdoutLogStream LogStream = "stdout"
	StderrLogStream LogStream = "stderr"
)

type RunRequest struct {
	Bundle chamberBundle.ProvisionedBundle
	Stdin  io.Reader
}

type Process interface {
	Wait() (exitCode int, err error)
}

type Runtime interface {
	Descriptor() Descriptor

	Binary() Binary

	// Run starts the OCI runtime process. Wait observes or returns its cached
	// exit result.
	Run(ctx context.Context, request RunRequest) (Process, error)

	State(ctx context.Context, containerID string) (ContainerState, error)

	Signal(ctx context.Context, request SignalRequest) error

	Delete(ctx context.Context, request DeleteRequest) error

	ReadLog(containerID string, stream LogStream) ([]byte, error)
}

type ContainerState struct {
	ContainerID string
	Status      ContainerStatus
}

type SignalRequest struct {
	ContainerID string
	Signal      Signal
}

type DeleteRequest struct {
	ContainerID string
	Force       bool
}

// Register attaches a constructor for a known runtime implementation.
// Runtime implementation packages call Register from init.
func Register(name string, newRuntime func(context.Context, Config, localfs.DirectoryManager) (Runtime, error)) {
	if newRuntime == nil {
		panic("runtime: Register constructor is nil")
	}
	implementation, ok := implementations[name]
	if !ok {
		panic("runtime: Register unknown implementation " + name)
	}
	if implementation.New != nil {
		panic("runtime: Register called twice for implementation " + name)
	}
	implementation.New = newRuntime
	implementations[name] = implementation
}

func New(ctx context.Context, config Config, directoryManager localfs.DirectoryManager) (Runtime, error) {
	return newForGOOS(ctx, config, directoryManager, goruntime.GOOS)
}

func newForGOOS(ctx context.Context, config Config, directoryManager localfs.DirectoryManager, goos string) (Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if directoryManager == nil {
		return nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Name == "" {
		return nil, fmt.Errorf("%w: runtime name is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Privilege == "" {
		return nil, fmt.Errorf("%w: runtime privilege is required", chamberErrors.ErrInvalidRequest)
	}
	implementation, ok := implementations[config.Name]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported runtime name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedNames(), ", "))
	}
	if !supportsPrivilege(implementation.Capabilities, config.Privilege) {
		return nil, fmt.Errorf("%w: %s runtime does not support %q privilege", chamberErrors.ErrInvalidRequest, config.Name, config.Privilege)
	}
	if goos != "linux" {
		return nil, fmt.Errorf("Chamber runtime requires a Linux host or Linux VM; current GOOS is %q", goos)
	}
	if implementation.New == nil {
		return nil, fmt.Errorf("runtime implementation %q is not linked into this binary", config.Name)
	}
	if config.RuntimeRoot == "" {
		return nil, fmt.Errorf("%w: runtime root is required", chamberErrors.ErrInvalidRequest)
	}
	if config.RuntimeBinDir == "" {
		return nil, fmt.Errorf("%w: runtime bin dir is required", chamberErrors.ErrInvalidRequest)
	}
	if err := directoryManager.MkdirPrivate(config.RuntimeRoot); err != nil {
		return nil, fmt.Errorf("create runtime root: %w", err)
	}
	if err := directoryManager.MkdirPrivate(config.RuntimeBinDir); err != nil {
		return nil, fmt.Errorf("create runtime bin dir: %w", err)
	}

	return implementation.New(ctx, config, directoryManager)
}

func SupportedNames() []string {
	names := make([]string, 0, len(implementations))
	for name := range implementations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func IsSupportedName(name string) bool {
	_, ok := implementations[name]
	return ok
}

func SupportedCapabilities(name string) (Capabilities, bool) {
	implementation, ok := implementations[name]
	if !ok {
		return Capabilities{}, false
	}
	return cloneCapabilities(implementation.Capabilities), true
}

func supportsPrivilege(capabilities Capabilities, privilege capability.Privilege) bool {
	for _, supported := range capabilities.Privileges {
		if supported == privilege {
			return true
		}
	}
	return false
}

func cloneCapabilities(capabilities Capabilities) Capabilities {
	return Capabilities{
		Privileges: append([]capability.Privilege(nil), capabilities.Privileges...),
		Isolation:  append([]Isolation(nil), capabilities.Isolation...),
	}
}
