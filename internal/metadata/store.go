package metadata

import (
	"context"
	"errors"
	"time"
)

type ErrorCode string

func (e ErrorCode) Error() string {
	return string(e)
}

const (
	ErrInvalidRequest       ErrorCode = "invalid_request"
	ErrImageNotFound        ErrorCode = "image_not_found"
	ErrPullFailed           ErrorCode = "pull_failed"
	ErrMetadataFailed       ErrorCode = "metadata_failed"
	ErrContainerNotFound    ErrorCode = "container_not_found"
	ErrLogNotFound          ErrorCode = "log_not_found"
	ErrBundlePrepareFailed  ErrorCode = "bundle_prepare_failed"
	ErrRuntimeStartFailed   ErrorCode = "runtime_start_failed"
	ErrRuntimeWaitFailed    ErrorCode = "runtime_wait_failed"
	ErrContainerExitNonzero ErrorCode = "container_exit_nonzero"
	ErrStateConflict        ErrorCode = "state_conflict"
)

var (
	ErrNotFound      = errors.New("metadata: not found")
	ErrAlreadyExists = errors.New("metadata: already exists")
)

type Image struct {
	// Reference is the user-facing name, for example
	// docker.io/library/alpine:latest.
	Reference string `json:"reference"`

	// Digest is the immutable manifest digest resolved by the puller.
	Digest string `json:"digest"`

	// LayoutPath is an absolute path to an OCI image-layout directory.
	LayoutPath string `json:"layout_path"`

	PulledAt   time.Time `json:"pulled_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type ContainerState string

const (
	ContainerCreating ContainerState = "creating"
	ContainerStarting ContainerState = "starting"
	ContainerRunning  ContainerState = "running"
	ContainerExited   ContainerState = "exited"
	ContainerFailed   ContainerState = "failed"
)

type Container struct {
	ID          string         `json:"id"`
	OperationID string         `json:"operation_id"`
	TraceID     string         `json:"trace_id,omitempty"`
	SpanID      string         `json:"span_id,omitempty"`
	ImageDigest string         `json:"image_digest"`
	ImageRef    string         `json:"image_ref"`
	BundlePath  string         `json:"bundle_path"`
	Runtime     string         `json:"runtime"`
	State       ContainerState `json:"state"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	ExitCode    *int           `json:"exit_code,omitempty"`
	ErrorCode   ErrorCode      `json:"error_code,omitempty"`
}

type OperationKind string

const (
	PullOperation OperationKind = "pull"
	RunOperation  OperationKind = "run"
)

type OperationState string

const (
	OperationRunning   OperationState = "running"
	OperationSucceeded OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
	OperationAborted   OperationState = "aborted"
)

type Operation struct {
	ID         string         `json:"id"`
	Kind       OperationKind  `json:"kind"`
	State      OperationState `json:"state"`
	ResourceID string         `json:"resource_id"`
	TraceID    string         `json:"trace_id,omitempty"`
	SpanID     string         `json:"span_id,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	ErrorCode  ErrorCode      `json:"error_code,omitempty"`
}

type StateTransition[T ~string] struct {
	From T
	To   T
}

var validContainerTransitions = map[StateTransition[ContainerState]]bool{
	{ContainerCreating, ContainerStarting}: true,
	{ContainerCreating, ContainerFailed}:   true,
	{ContainerStarting, ContainerRunning}:  true,
	{ContainerStarting, ContainerFailed}:   true,
	{ContainerStarting, ContainerExited}:   true,
	{ContainerRunning, ContainerExited}:    true,
	{ContainerRunning, ContainerFailed}:    true,
}

var validOperationTransitions = map[StateTransition[OperationState]]bool{
	{OperationRunning, OperationSucceeded}: true,
	{OperationRunning, OperationFailed}:    true,
	{OperationRunning, OperationAborted}:   true,
}

func IsContainerTransitionValid(from, to ContainerState) bool {
	return validContainerTransitions[StateTransition[ContainerState]{from, to}]
}

func IsOperationTransitionValid(from, to OperationState) bool {
	return validOperationTransitions[StateTransition[OperationState]{from, to}]
}

type Store interface {
	PutImage(ctx context.Context, image Image) error
	GetImage(ctx context.Context, reference string) (Image, error)

	CreateOperation(ctx context.Context, operation Operation) error
	GetOperation(ctx context.Context, id string) (Operation, error)
	TransitionOperation(
		ctx context.Context,
		id string,
		from OperationState,
		update OperationUpdate,
	) (Operation, error)

	CreateContainer(ctx context.Context, container Container) error
	GetContainer(ctx context.Context, id string) (Container, error)
	ListContainers(ctx context.Context) ([]Container, error)
	TransitionContainer(
		ctx context.Context,
		id string,
		from ContainerState,
		update ContainerUpdate,
	) (Container, error)

	Close() error
}

type ContainerUpdate struct {
	State     ContainerState
	At        time.Time
	ExitCode  *int
	ErrorCode string
}

type OperationUpdate struct {
	State     OperationState
	At        time.Time
	ErrorCode string
}
