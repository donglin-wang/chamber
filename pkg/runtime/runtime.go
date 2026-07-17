package runtime

import (
	"context"
	"io"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
)

type Binary struct {
	Name    string
	Version string
	Path    string
}

type RunRequest struct {
	Bundle chamberBundle.ProvisionedBundle
	Stdin  io.Reader
}

type Process interface {
	Wait() (exitCode int, err error)
}

type Runtime interface {
	// Ensure prepares the runtime implementation for future Run calls.
	Ensure(ctx context.Context) (Binary, error)

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
