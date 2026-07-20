package shared

import (
	"context"
	"io"
	"os"

	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
)

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
	BinaryPath   string
	Capabilities Capabilities
}

type ContainerStatus string

const (
	ContainerStatusCreating ContainerStatus = "creating"
	ContainerStatusCreated  ContainerStatus = "created"
	ContainerStatusRunning  ContainerStatus = "running"
	ContainerStatusStopped  ContainerStatus = "stopped"
)

type LogStream string

const (
	StdoutLogStream LogStream = "stdout"
	StderrLogStream LogStream = "stderr"
)

type RunRequest struct {
	Bundle chamberBundleShared.ProvisionedBundle
	Stdin  io.Reader
	Stdout []io.Writer
	Stderr []io.Writer
}

type ContainerResult struct {
	ExitCode int
}

type Container interface {
	ID() string
	PID() int
	StdoutPath() string
	StderrPath() string
	Wait() (ContainerResult, error)
	State(ctx context.Context) (ContainerState, error)
	Signal(ctx context.Context, signal os.Signal) error
	Delete(ctx context.Context, force bool) error
	ReadLog(stream LogStream) ([]byte, error)
	DeleteLog(stream LogStream) error
}

type Runtime interface {
	Descriptor() Descriptor

	Run(ctx context.Context, request RunRequest) (Container, error)
}

type ContainerState struct {
	ContainerID string
	Status      ContainerStatus
}
