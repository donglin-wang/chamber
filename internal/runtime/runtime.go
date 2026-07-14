package runtime

import (
	"context"
	"io"

	chbundle "github.com/donglin-wang/chamber/internal/bundle"
)

type Binary struct {
	Name    string
	Version string
	Path    string
}

type RunRequest struct {
	Bundle chbundle.ProvisionedBundle
	Stdin  io.Reader
}

type Process interface {
	Wait() (exitCode int, err error)
}

type ObservedState string

const (
	ProcessRunning ObservedState = "running"
	ProcessExited  ObservedState = "exited"
)

type StartResult struct {
	Process Process
	State   ObservedState
}

type Runtime interface {
	// Ensure prepares the runtime implementation for future Run calls.
	Ensure(ctx context.Context) (Binary, error)

	// Run starts the OCI runtime process and returns only after the child has
	// either reached "running" or exited before that state could be observed.
	// Wait observes or returns its cached exit result.
	Run(ctx context.Context, request RunRequest) (StartResult, error)

	ReadLog(containerID string, stream string) ([]byte, error)
}

const (
	StdoutLogStream = "stdout"
	StderrLogStream = "stderr"
)
