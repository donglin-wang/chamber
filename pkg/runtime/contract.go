package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
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
	// ContainerID is the caller-chosen runtime identity for the container.
	ContainerID string

	// BundlePath is the ready OCI runtime bundle directory to run.
	BundlePath string

	// Stdin is connected to the container's standard input when non-nil.
	Stdin io.Reader

	// Stdout receives copies of container stdout in addition to the default
	// Chamber log file.
	Stdout []io.Writer

	// Stderr receives copies of container stderr in addition to the default
	// Chamber log file.
	Stderr []io.Writer
}

// RunImageRequest configures execution of an image without manual provisioning.
type RunImageRequest struct {
	// ContainerID optionally selects the runtime identity. Empty generates a
	// unique ID, available through Container.ID.
	ContainerID string

	// Process overrides the image's process defaults. Terminal defaults to false
	// because the runtime supports non-interactive execution only.
	Process chamberBundle.ProcessSpec

	// Mounts adds filesystem mounts to the provisioned bundle.
	Mounts []chamberBundle.Mount

	// Stdin, Stdout, and Stderr have the same meaning as in RunRequest.
	Stdin  io.Reader
	Stdout []io.Writer
	Stderr []io.Writer
}

// ContainerResult is the result returned after Chamber waits for a container
// process or maps a launch failure.
type ContainerResult struct {
	ContainerID string                `json:"container_id"`
	Status      ContainerResultStatus `json:"status"`
	ExitCode    *int                  `json:"exit_code,omitempty"`
	ErrorCode   chamberErrors.Code    `json:"error_code,omitempty"`
	Error       string                `json:"error,omitempty"`
	StartedAt   *time.Time            `json:"started_at,omitempty"`
	ExitedAt    time.Time             `json:"exited_at"`
}

// ContainerResultStatus is the coarse outcome of a container run.
type ContainerResultStatus string

const (
	// ContainerResultStatusExited means the container process exited and reported
	// an exit code.
	ContainerResultStatusExited ContainerResultStatus = "exited"

	// ContainerResultStatusStartFailed means runtime launch failed before a
	// container was confirmed started.
	ContainerResultStatusStartFailed ContainerResultStatus = "start_failed"

	// ContainerResultStatusCanceled means waiting was canceled.
	ContainerResultStatusCanceled ContainerResultStatus = "canceled"

	// ContainerResultStatusUnknown means the container started, but Chamber could
	// not determine a clean exited/canceled result.
	ContainerResultStatusUnknown ContainerResultStatus = "unknown"
)

// ContainerHandle controls and reads logs for one existing runtime container.
type ContainerHandle interface {
	// ID returns the runtime container identity.
	ID() string

	// StdoutPath returns the default stdout log path.
	StdoutPath() string

	// StderrPath returns the default stderr log path.
	StderrPath() string

	// State reads the current runtime state for the container.
	State(ctx context.Context) (ContainerState, error)

	// Signal sends signal to the container through the runtime.
	Signal(ctx context.Context, signal os.Signal) error

	// Delete removes runtime state for the container. If force is true, the
	// runtime may terminate a still-running container as part of deletion.
	// For a Container returned by RunImage, successful deletion also removes
	// its runtime-owned bundle. Logs remain available until DeleteLog is called.
	Delete(ctx context.Context, force bool) error

	// ReadLog reads one default runtime log stream.
	ReadLog(stream LogStream) ([]byte, error)

	// DeleteLog removes one default runtime log stream.
	DeleteLog(stream LogStream) error
}

// Container owns the wait lifecycle for one container started by this process.
type Container interface {
	ContainerHandle

	// Wait waits for the container process to exit and releases launch-time
	// resources owned by Chamber. If ctx is canceled before the process exits,
	// the runtime may terminate the container before returning.
	Wait(ctx context.Context) (ContainerResult, error)
}

// Runtime runs local images and ready OCI bundles.
type Runtime interface {
	// Descriptor returns implementation identity and artifact paths.
	Descriptor() Descriptor

	// RunImage provisions the local image and starts a container. The runtime
	// owns the provisioner and bundle workspace. Call Wait to observe completion
	// and Delete to release runtime state and the bundle, including after a
	// canceled Wait. Launch failures release the bundle before returning.
	// The context controls provisioning and launch work only.
	// Chamber provisioning permits nested rootless containers by default.
	RunImage(ctx context.Context, image chamberImage.Image, request RunImageRequest) (Container, error)

	// Run starts the container and returns a Container that owns subsequent
	// lifecycle operations. The context controls launch work only; after Run
	// succeeds, callers stop or clean up the container through Container methods.
	// The bundle is executed as supplied and remains caller-owned. Callers using
	// externally prepared bundles own their nested-container configuration.
	Run(ctx context.Context, request RunRequest) (Container, error)

	// Open returns a handle for an existing container owned by this runtime
	// root. Reopened handles support control and logs, but not Wait or cleanup
	// of bundles owned by the original RunImage Container.
	Open(ctx context.Context, containerID string) (ContainerHandle, error)
}

// RunAndWait starts a container, optionally reports the started handle, waits
// for the process to exit, and returns one normalized container result.
func RunAndWait(ctx context.Context, rt Runtime, request RunRequest, started func(ContainerHandle) error) (ContainerResult, error) {
	if rt == nil {
		err := fmt.Errorf("%w: runtime is required", chamberErrors.ErrInvalidRequest)
		return ContainerResult{
			ContainerID: request.ContainerID,
			Status:      ContainerResultStatusStartFailed,
			ErrorCode:   chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeStartFailed),
			Error:       err.Error(),
			ExitedAt:    time.Now().UTC(),
		}, err
	}

	container, err := rt.Run(ctx, request)
	if err != nil {
		return ContainerResult{
			ContainerID: request.ContainerID,
			Status:      ContainerResultStatusStartFailed,
			ErrorCode:   chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeStartFailed),
			Error:       err.Error(),
			ExitedAt:    time.Now().UTC(),
		}, err
	}

	startedAt := time.Now().UTC()
	if started != nil {
		if err := started(container); err != nil {
			_ = container.Delete(context.Background(), true)
			return ContainerResult{
				ContainerID: container.ID(),
				Status:      ContainerResultStatusUnknown,
				ErrorCode:   chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeWaitFailed),
				Error:       err.Error(),
				StartedAt:   &startedAt,
				ExitedAt:    time.Now().UTC(),
			}, err
		}
	}

	result, waitErr := container.Wait(ctx)
	result.ContainerID = container.ID()
	result.StartedAt = &startedAt
	if waitErr == nil {
		return result, nil
	}
	if errors.Is(waitErr, chamberErrors.ErrCanceled) {
		result.Status = ContainerResultStatusCanceled
		result.ErrorCode = chamberErrors.CodeFromError(waitErr, chamberErrors.ErrCanceled)
		result.Error = waitErr.Error()
		return result, waitErr
	}
	result.Status = ContainerResultStatusUnknown
	result.ErrorCode = chamberErrors.CodeFromError(waitErr, chamberErrors.ErrRuntimeWaitFailed)
	result.Error = waitErr.Error()
	return result, waitErr
}

// ContainerState is a point-in-time runtime state snapshot for a container.
type ContainerState struct {
	// ContainerID is the runtime container ID.
	ContainerID string

	// Status is the runtime status mapped into Chamber's public vocabulary.
	Status ContainerStatus
}
