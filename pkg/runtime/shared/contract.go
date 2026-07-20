package shared

import (
	"context"
	"io"

	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
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
	Bundle chamberBundleShared.ProvisionedBundle
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
