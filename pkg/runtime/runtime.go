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

	ReadLog(containerID string, stream string) ([]byte, error)
}

type ContainerState struct {
	ContainerID string
	Status      string
}

type SignalRequest struct {
	ContainerID string
	Signal      string
}

type DeleteRequest struct {
	ContainerID string
	Force       bool
}

const (
	StdoutLogStream = "stdout"
	StderrLogStream = "stderr"
)

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
		return nil, fmt.Errorf("context is required")
	}
	if directoryManager == nil {
		return nil, fmt.Errorf("directory manager is required")
	}
	if config.Name == "" {
		config.Name = RuntimeNameRunc
	}
	if config.Privilege == "" {
		config.Privilege = capability.Rootless
	}
	resolved, err := Resolve(config, Override{})
	if err != nil {
		return nil, err
	}

	implementation, ok := implementations[resolved.Name]
	if !ok {
		return nil, fmt.Errorf("unsupported runtime name %q (supported: %s)", resolved.Name, strings.Join(SupportedNames(), ", "))
	}
	if !supportsPrivilege(implementation.Capabilities, resolved.Privilege) {
		return nil, fmt.Errorf("%s runtime does not support %q privilege", resolved.Name, resolved.Privilege)
	}
	if goos != "linux" {
		return nil, fmt.Errorf("Chamber runtime requires a Linux host or Linux VM; current GOOS is %q", goos)
	}
	if implementation.New == nil {
		return nil, fmt.Errorf("runtime implementation %q is not linked into this binary", resolved.Name)
	}
	if resolved.RuntimeRoot == "" {
		return nil, fmt.Errorf("runtime root is required")
	}
	if resolved.RuntimeBinDir == "" {
		return nil, fmt.Errorf("runtime bin dir is required")
	}
	if err := directoryManager.MkdirPrivate(resolved.RuntimeRoot); err != nil {
		return nil, fmt.Errorf("create runtime root: %w", err)
	}
	if err := directoryManager.MkdirPrivate(resolved.RuntimeBinDir); err != nil {
		return nil, fmt.Errorf("create runtime bin dir: %w", err)
	}

	return implementation.New(ctx, resolved, directoryManager)
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
