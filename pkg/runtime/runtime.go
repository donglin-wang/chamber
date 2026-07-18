package runtime

import (
	"context"
	"io"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
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
