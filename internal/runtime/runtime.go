package runtime

import (
	"context"
	"io"
)

type Binary struct {
	Name    string
	Version string
	Path    string
}

type RunRequest struct {
	ID         string
	BundlePath string
	StateRoot  string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
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

type PrepareRequest struct {
	ContainerID string
	ImageLayout string
	ImageRef    string
	Command     []string
}

type BundlePreparer interface {
	Prepare(
		ctx context.Context,
		request PrepareRequest,
	) (bundlePath string, err error)
}

type Runtime interface {
	// Ensure downloads the configured binary from a trusted HTTPS source when
	// absent, verifies its SHA-256 checksum, and returns its absolute path.
	Ensure(ctx context.Context) (Binary, error)

	// Run starts the OCI runtime process and returns only after the child has
	// either reached "running" or exited before that state could be observed.
	// Wait observes or returns its cached exit result.
	Run(ctx context.Context, binary Binary, request RunRequest) (StartResult, error)
}
