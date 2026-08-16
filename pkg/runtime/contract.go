package runtime

import (
	"context"
	"io"
	"os"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
)

// Descriptor identifies a ready runtime implementation.
type Descriptor struct {
	// Name is the runtime implementation name.
	Name string

	// Version is the runtime binary or implementation version when available.
	Version string

	// BinaryPath is the runtime binary path used by this runtime, when the
	// implementation uses a host binary.
	BinaryPath string
}

// ContainerStatus is Chamber's public runtime container-state vocabulary.
type ContainerStatus string

const (
	// ContainerStatusCreating means the runtime is creating the container.
	ContainerStatusCreating ContainerStatus = "creating"

	// ContainerStatusCreated means the container exists but is not running.
	ContainerStatusCreated ContainerStatus = "created"

	// ContainerStatusRunning means the container process is running.
	ContainerStatusRunning ContainerStatus = "running"

	// ContainerStatusStopped means the container has stopped.
	ContainerStatusStopped ContainerStatus = "stopped"
)

// LogStream selects one of Chamber's default runtime log streams.
type LogStream string

const (
	// StdoutLogStream selects the container stdout log.
	StdoutLogStream LogStream = "stdout"

	// StderrLogStream selects the container stderr log.
	StderrLogStream LogStream = "stderr"
)

// RunRequest describes one container launch from a provisioned bundle.
type RunRequest struct {
	// Bundle is the provisioned OCI runtime bundle to run.
	Bundle chamberBundle.ProvisionedBundle

	// Stdin is connected to the container's standard input when non-nil.
	Stdin io.Reader

	// Stdout receives copies of container stdout in addition to the default
	// Chamber log file.
	Stdout []io.Writer

	// Stderr receives copies of container stderr in addition to the default
	// Chamber log file.
	Stderr []io.Writer
}

// ContainerResult is the result returned after a container process exits.
type ContainerResult struct {
	// ExitCode is the process exit code reported by the runtime.
	ExitCode int
}

// Container owns the lifecycle controls and logs for one started container.
type Container interface {
	// ID returns the container ID supplied when the bundle was provisioned.
	ID() string

	// StdoutPath returns the default stdout log path.
	StdoutPath() string

	// StderrPath returns the default stderr log path.
	StderrPath() string

	// Wait waits for the container process to exit and releases launch-time
	// resources owned by Chamber. If ctx is canceled before the process exits,
	// the runtime may terminate the container before returning.
	Wait(ctx context.Context) (ContainerResult, error)

	// State reads the current runtime state for the container.
	State(ctx context.Context) (ContainerState, error)

	// Signal sends signal to the container through the runtime.
	Signal(ctx context.Context, signal os.Signal) error

	// Delete removes runtime state for the container. If force is true, the
	// runtime may terminate a still-running container as part of deletion.
	Delete(ctx context.Context, force bool) error

	// ReadLog reads one default runtime log stream.
	ReadLog(stream LogStream) ([]byte, error)

	// DeleteLog removes one default runtime log stream.
	DeleteLog(stream LogStream) error
}

// Runtime starts provisioned bundles.
type Runtime interface {
	// Descriptor returns implementation identity and artifact paths.
	Descriptor() Descriptor

	// Run starts the container and returns a Container that owns subsequent
	// lifecycle operations. The context controls launch work only; after Run
	// succeeds, callers stop or clean up the container through Container methods.
	Run(ctx context.Context, request RunRequest) (Container, error)
}

// ContainerState is a point-in-time runtime state snapshot for a container.
type ContainerState struct {
	// ContainerID is the runtime container ID.
	ContainerID string

	// Status is the runtime status mapped into Chamber's public vocabulary.
	Status ContainerStatus
}
