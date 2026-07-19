package runtime

import (
	"context"
	"fmt"
	"io"
	goruntime "runtime"
	"strings"
	"sync"

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

var runtimeNames = [...]string{
	RuntimeNameRunc,
}

type Constructor func(context.Context, Config, localfs.DirectoryManager) (Runtime, error)

type Implementation struct {
	Name         string
	Capabilities Capabilities
	New          Constructor
}

var (
	implementationsMu sync.RWMutex
	implementations   = map[string]Implementation{
		RuntimeNameRunc: {
			Name: RuntimeNameRunc,
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
)

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

func Register(name string, constructor Constructor) {
	implementationsMu.Lock()
	defer implementationsMu.Unlock()

	if constructor == nil {
		panic("runtime: Register constructor is nil")
	}
	implementation, ok := implementations[name]
	if !ok {
		panic("runtime: Register unknown implementation " + name)
	}
	if implementation.New != nil {
		panic("runtime: Register called twice for implementation " + name)
	}
	implementation.New = constructor
	implementations[name] = implementation
}

func New(ctx context.Context, config Config, directoryManager localfs.DirectoryManager) (Runtime, error) {
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

	implementation, ok := lookupImplementation(resolved.Name)
	if !ok {
		return nil, fmt.Errorf("unsupported runtime name %q (supported: %s)", resolved.Name, strings.Join(SupportedNames(), ", "))
	}
	if !supportsPrivilege(implementation.Capabilities, resolved.Privilege) {
		return nil, fmt.Errorf("%s runtime does not support %q privilege", resolved.Name, resolved.Privilege)
	}
	if goruntime.GOOS != "linux" {
		return nil, fmt.Errorf("Chamber runtime requires a Linux host or Linux VM; current GOOS is %q", goruntime.GOOS)
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
	names := make([]string, len(runtimeNames))
	copy(names, runtimeNames[:])
	return names
}

func IsSupportedName(name string) bool {
	_, ok := lookupImplementation(name)
	return ok
}

func SupportedCapabilities(name string) (Capabilities, bool) {
	implementation, ok := lookupImplementation(name)
	if !ok {
		return Capabilities{}, false
	}
	return cloneCapabilities(implementation.Capabilities), true
}

func lookupImplementation(name string) (Implementation, bool) {
	implementationsMu.RLock()
	defer implementationsMu.RUnlock()

	implementation, ok := implementations[name]
	return implementation, ok
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
