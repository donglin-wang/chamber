package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	chbundle "github.com/donglin-wang/chamber/internal/bundle"
	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/donglin-wang/chamber/internal/metadata"
	chruntime "github.com/donglin-wang/chamber/internal/runtime"
	"github.com/donglin-wang/chamber/internal/shared/localfs"
)

type Clock interface {
	Now() time.Time
}

type IDGenerator interface {
	New() string
}

type Correlator interface {
	TraceIDs(ctx context.Context) (traceID string, spanID string)
}

type Service struct {
	Store         metadata.Store
	Puller        chimage.Puller
	Runtime       chruntime.Runtime
	Binary        chruntime.Binary
	Provisioner   chbundle.Provisioner
	Clock         Clock
	IDs           IDGenerator
	Lifetime      context.Context
	Correlator    Correlator
	Logger        *slog.Logger
	Directories   localfs.DirectoryManager
	ImageRoot     string
	RuntimeRoot   string
	ContainerRoot string
	Platform      string
}

type PullRequest struct {
	Reference string
}

type RunRequest struct {
	Image   string
	Command []string
}

type PullResult struct {
	Operation metadata.Operation
	Image     metadata.Image
}

type RunResult struct {
	Operation metadata.Operation
	Container metadata.Container
}

type Error struct {
	OperationID string
	Code        string
	Err         error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func (s *Service) Pull(
	ctx context.Context,
	request PullRequest,
) (PullResult, error) {
	if err := s.requirePullDependencies(); err != nil {
		return PullResult{}, err
	}

	traceID, spanID := s.traceIDs(ctx)
	startedAt := s.now()
	operationID := s.IDs.New()
	operation := metadata.Operation{
		ID:         operationID,
		Kind:       metadata.PullOperation,
		State:      metadata.OperationRunning,
		ResourceID: request.Reference,
		TraceID:    traceID,
		SpanID:     spanID,
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := s.Store.CreateOperation(ctx, operation); err != nil {
		return PullResult{}, fmt.Errorf("create pull operation: %w", err)
	}

	s.logInfo("pull started", "operation_id", operationID)

	reference := strings.TrimSpace(request.Reference)
	if reference == "" {
		err := fmt.Errorf("image reference is required")
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrInvalidRequest, err)
		return PullResult{Operation: chooseOperation(failed, operation)}, failErr
	}

	destination, err := s.imageDestination(reference)
	if err != nil {
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrInvalidRequest, err)
		return PullResult{Operation: chooseOperation(failed, operation)}, failErr
	}

	pulled, err := s.Puller.Pull(ctx, chimage.PullRequest{
		Reference:   reference,
		Destination: destination,
		Platform:    s.Platform,
	})
	if err != nil {
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrPullFailed, err)
		return PullResult{Operation: chooseOperation(failed, operation)}, failErr
	}

	pulledAt := pulled.PulledAt
	if pulledAt.IsZero() {
		pulledAt = s.now()
	}
	image := metadata.Image{
		Reference:  reference,
		Digest:     pulled.Digest,
		LayoutPath: firstNonEmpty(pulled.LayoutPath, destination),
		PulledAt:   pulledAt,
		LastUsedAt: pulledAt,
	}
	if err := s.Store.PutImage(ctx, image); err != nil {
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrMetadataFailed, err)
		return PullResult{Operation: chooseOperation(failed, operation), Image: image}, failErr
	}

	completed, err := s.succeedOperation(ctx, operationID)
	if err != nil {
		return PullResult{Operation: operation, Image: image}, s.daemonError(operationID, metadata.ErrMetadataFailed, err)
	}

	s.logInfo("pull completed", "operation_id", operationID)
	return PullResult{
		Operation: completed,
		Image:     image,
	}, nil
}

func (s *Service) Run(
	ctx context.Context,
	request RunRequest,
) (RunResult, error) {
	if err := s.requireRunDependencies(); err != nil {
		return RunResult{}, err
	}

	traceID, spanID := s.traceIDs(ctx)
	startedAt := s.now()
	operationID := s.IDs.New()
	containerID := s.IDs.New()
	operation := metadata.Operation{
		ID:         operationID,
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: containerID,
		TraceID:    traceID,
		SpanID:     spanID,
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := s.Store.CreateOperation(ctx, operation); err != nil {
		return RunResult{}, fmt.Errorf("create run operation: %w", err)
	}

	if strings.TrimSpace(request.Image) == "" {
		err := fmt.Errorf("image reference is required")
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrInvalidRequest, err)
		return RunResult{Operation: chooseOperation(failed, operation)}, failErr
	}
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		err := fmt.Errorf("command is required")
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrInvalidRequest, err)
		return RunResult{Operation: chooseOperation(failed, operation)}, failErr
	}

	image, err := s.Store.GetImage(ctx, request.Image)
	if err != nil {
		code := metadata.ErrMetadataFailed
		if errors.Is(err, metadata.ErrNotFound) {
			code = metadata.ErrImageNotFound
		}
		failed, failErr := s.failOperation(ctx, operationID, code, err)
		return RunResult{Operation: chooseOperation(failed, operation)}, failErr
	}

	bundlePath, err := s.containerPath(containerID)
	if err != nil {
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrInvalidRequest, err)
		return RunResult{Operation: chooseOperation(failed, operation)}, failErr
	}

	// metadata.Store has no non-state update for BundlePath. The service chooses
	// the provisioner's final deterministic path before creating the record.
	container := metadata.Container{
		ID:          containerID,
		OperationID: operationID,
		TraceID:     traceID,
		SpanID:      spanID,
		ImageDigest: image.Digest,
		ImageRef:    image.Reference,
		BundlePath:  bundlePath,
		Runtime:     s.Binary.Name,
		State:       metadata.ContainerCreating,
		CreatedAt:   s.now(),
		UpdatedAt:   s.now(),
	}
	if err := s.Store.CreateContainer(ctx, container); err != nil {
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrMetadataFailed, err)
		return RunResult{Operation: chooseOperation(failed, operation)}, failErr
	}

	provisioned, err := s.Provisioner.Provision(ctx, chbundle.ProvisionRequest{
		ContainerID: containerID,
		ImageLayout: image.LayoutPath,
		ImageRef:    image.Reference,
		Command:     request.Command,
	})
	if err != nil {
		failedContainer, failedOp, failErr := s.failContainerAndOperation(
			ctx,
			containerID,
			metadata.ContainerCreating,
			operationID,
			metadata.ErrBundlePrepareFailed,
			err,
		)
		return RunResult{
			Operation: chooseOperation(failedOp, operation),
			Container: chooseContainer(failedContainer, container),
		}, failErr
	}
	if provisioned.BundlePath == "" {
		provisioned.BundlePath = bundlePath
	}

	starting, err := s.Store.TransitionContainer(ctx, containerID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State: metadata.ContainerStarting,
		At:    s.now(),
	})
	if err != nil {
		failed, failErr := s.failOperation(ctx, operationID, metadata.ErrMetadataFailed, err)
		return RunResult{Operation: chooseOperation(failed, operation), Container: container}, failErr
	}

	stdout, stderr, err := s.openContainerLogs(provisioned.BundlePath)
	if err != nil {
		failedContainer, failedOp, failErr := s.failContainerAndOperation(
			ctx,
			containerID,
			metadata.ContainerStarting,
			operationID,
			metadata.ErrRuntimeStartFailed,
			err,
		)
		return RunResult{
			Operation: chooseOperation(failedOp, operation),
			Container: chooseContainer(failedContainer, starting),
		}, failErr
	}

	start, err := s.Runtime.Run(s.lifetime(), s.Binary, chruntime.RunRequest{
		Bundle:    provisioned,
		StateRoot: s.RuntimeRoot,
		Stdout:    stdout,
		Stderr:    stderr,
	})
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		failedContainer, failedOp, failErr := s.failContainerAndOperation(
			ctx,
			containerID,
			metadata.ContainerStarting,
			operationID,
			metadata.ErrRuntimeStartFailed,
			err,
		)
		return RunResult{
			Operation: chooseOperation(failedOp, operation),
			Container: chooseContainer(failedContainer, starting),
		}, failErr
	}

	switch start.State {
	case chruntime.ProcessRunning:
		running, err := s.Store.TransitionContainer(ctx, containerID, metadata.ContainerStarting, metadata.ContainerUpdate{
			State: metadata.ContainerRunning,
			At:    s.now(),
		})
		if err != nil {
			go s.finishRunningProcess(operationID, containerID, metadata.ContainerStarting, start.Process, stdout, stderr)
			return RunResult{
				Operation: operation,
				Container: starting,
			}, s.daemonError(operationID, metadata.ErrMetadataFailed, err)
		}
		go s.finishRunningProcess(operationID, containerID, metadata.ContainerRunning, start.Process, stdout, stderr)
		return RunResult{
			Operation: operation,
			Container: running,
		}, nil
	case chruntime.ProcessExited:
		exited, completed, err := s.finishExitedProcess(ctx, operationID, containerID, metadata.ContainerStarting, start.Process)
		_ = stdout.Close()
		_ = stderr.Close()
		return RunResult{
			Operation: chooseOperation(completed, operation),
			Container: chooseContainer(exited, starting),
		}, err
	default:
		_ = stdout.Close()
		_ = stderr.Close()
		err := fmt.Errorf("runtime returned unknown observed state %q", start.State)
		failedContainer, failedOp, failErr := s.failContainerAndOperation(
			ctx,
			containerID,
			metadata.ContainerStarting,
			operationID,
			metadata.ErrRuntimeStartFailed,
			err,
		)
		return RunResult{
			Operation: chooseOperation(failedOp, operation),
			Container: chooseContainer(failedContainer, starting),
		}, failErr
	}
}

func (s *Service) finishRunningProcess(
	operationID string,
	containerID string,
	from metadata.ContainerState,
	process chruntime.Process,
	stdout *os.File,
	stderr *os.File,
) {
	defer stdout.Close()
	defer stderr.Close()

	_, _, err := s.finishExitedProcess(context.Background(), operationID, containerID, from, process)
	if err != nil {
		s.logError("record container exit", err, "operation_id", operationID, "container_id", containerID)
	}
}

func (s *Service) finishExitedProcess(
	ctx context.Context,
	operationID string,
	containerID string,
	from metadata.ContainerState,
	process chruntime.Process,
) (metadata.Container, metadata.Operation, error) {
	if process == nil {
		err := fmt.Errorf("runtime process is required")
		failedContainer, failedOp, failErr := s.failContainerAndOperation(ctx, containerID, from, operationID, metadata.ErrRuntimeWaitFailed, err)
		return failedContainer, failedOp, failErr
	}

	exitCode, waitErr := process.Wait()
	exitCodePtr := &exitCode
	code := metadata.ErrorCode("")
	operationState := metadata.OperationSucceeded
	if waitErr != nil {
		code = metadata.ErrRuntimeWaitFailed
		operationState = metadata.OperationFailed
	} else if exitCode != 0 {
		code = metadata.ErrContainerExitNonzero
		operationState = metadata.OperationFailed
	}

	exited, containerErr := s.Store.TransitionContainer(ctx, containerID, from, metadata.ContainerUpdate{
		State:     metadata.ContainerExited,
		At:        s.now(),
		ExitCode:  exitCodePtr,
		ErrorCode: string(code),
	})
	if containerErr != nil {
		err := errors.Join(waitErr, containerErr)
		return exited, metadata.Operation{}, s.daemonError(operationID, metadata.ErrMetadataFailed, err)
	}

	var operation metadata.Operation
	var operationErr error
	if operationState == metadata.OperationSucceeded {
		operation, operationErr = s.succeedOperation(ctx, operationID)
	} else {
		operation, operationErr = s.transitionOperation(ctx, operationID, metadata.OperationFailed, code)
	}

	if operationErr != nil {
		err := errors.Join(waitErr, operationErr)
		if waitErr == nil && code != "" {
			err = errors.Join(fmt.Errorf("%s", code), operationErr)
		}
		return exited, operation, s.daemonError(operationID, firstNonZeroCode(code, metadata.ErrMetadataFailed), err)
	}
	if waitErr != nil {
		return exited, operation, s.daemonError(operationID, code, waitErr)
	}
	if exitCode != 0 {
		return exited, operation, s.daemonError(operationID, code, fmt.Errorf("container exited with status %d", exitCode))
	}
	return exited, operation, nil
}

func (s *Service) failContainerAndOperation(
	ctx context.Context,
	containerID string,
	from metadata.ContainerState,
	operationID string,
	code metadata.ErrorCode,
	cause error,
) (metadata.Container, metadata.Operation, error) {
	container, containerErr := s.Store.TransitionContainer(ctx, containerID, from, metadata.ContainerUpdate{
		State:     metadata.ContainerFailed,
		At:        s.now(),
		ErrorCode: string(code),
	})
	operation, operationErr := s.transitionOperation(ctx, operationID, metadata.OperationFailed, code)
	if containerErr != nil || operationErr != nil {
		cause = errors.Join(cause, containerErr, operationErr)
		s.logError("record failed run", cause, "operation_id", operationID, "container_id", containerID, "code", string(code))
	}
	return container, operation, s.daemonError(operationID, code, cause)
}

func (s *Service) failOperation(
	ctx context.Context,
	operationID string,
	code metadata.ErrorCode,
	cause error,
) (metadata.Operation, error) {
	operation, err := s.transitionOperation(ctx, operationID, metadata.OperationFailed, code)
	if err != nil {
		cause = errors.Join(cause, err)
		s.logError("record failed operation", cause, "operation_id", operationID, "code", string(code))
	}
	return operation, s.daemonError(operationID, code, cause)
}

func (s *Service) transitionOperation(
	ctx context.Context,
	operationID string,
	state metadata.OperationState,
	code metadata.ErrorCode,
) (metadata.Operation, error) {
	return s.Store.TransitionOperation(ctx, operationID, metadata.OperationRunning, metadata.OperationUpdate{
		State:     state,
		At:        s.now(),
		ErrorCode: string(code),
	})
}

func (s *Service) succeedOperation(ctx context.Context, operationID string) (metadata.Operation, error) {
	return s.transitionOperation(ctx, operationID, metadata.OperationSucceeded, "")
}

func (s *Service) daemonError(operationID string, code metadata.ErrorCode, err error) error {
	return &Error{
		OperationID: operationID,
		Code:        string(code),
		Err:         err,
	}
}

func (s *Service) requirePullDependencies() error {
	if s.Store == nil {
		return fmt.Errorf("metadata store is required")
	}
	if s.Puller == nil {
		return fmt.Errorf("image puller is required")
	}
	if s.IDs == nil {
		return fmt.Errorf("id generator is required")
	}
	return nil
}

func (s *Service) requireRunDependencies() error {
	if s.Store == nil {
		return fmt.Errorf("metadata store is required")
	}
	if s.Provisioner == nil {
		return fmt.Errorf("bundle provisioner is required")
	}
	if s.Runtime == nil {
		return fmt.Errorf("runtime is required")
	}
	if s.IDs == nil {
		return fmt.Errorf("id generator is required")
	}
	return nil
}

func (s *Service) traceIDs(ctx context.Context) (string, string) {
	if s.Correlator == nil {
		return "", ""
	}
	return s.Correlator.TraceIDs(ctx)
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}

func (s *Service) lifetime() context.Context {
	if s.Lifetime == nil {
		return context.Background()
	}
	return s.Lifetime
}

func (s *Service) imageDestination(reference string) (string, error) {
	if strings.TrimSpace(s.ImageRoot) == "" {
		return "", fmt.Errorf("image root is required")
	}
	sum := sha256.Sum256([]byte(reference))
	return filepath.Join(s.ImageRoot, hex.EncodeToString(sum[:])), nil
}

func (s *Service) containerPath(containerID string) (string, error) {
	root := s.ContainerRoot
	if root == "" && s.RuntimeRoot != "" {
		root = filepath.Join(s.RuntimeRoot, "containers")
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("container root is required")
	}
	return filepath.Join(root, containerID), nil
}

func (s *Service) logInfo(message string, args ...any) {
	if s.Logger == nil {
		return
	}
	s.Logger.Info(message, args...)
}

func (s *Service) logError(message string, err error, args ...any) {
	if s.Logger == nil {
		return
	}
	args = append(args, slog.Any("error", err))
	s.Logger.Error(message, args...)
}

func (s *Service) openContainerLogs(containerPath string) (*os.File, *os.File, error) {
	if containerPath == "" {
		return nil, nil, fmt.Errorf("container path is required")
	}
	if err := s.directoryManager().EnsurePrivateDir(containerPath); err != nil {
		return nil, nil, fmt.Errorf("prepare container log directory: %w", err)
	}
	stdout, err := os.OpenFile(filepath.Join(containerPath, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open stdout log: %w", err)
	}
	stderr, err := os.OpenFile(filepath.Join(containerPath, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("open stderr log: %w", err)
	}
	return stdout, stderr, nil
}

func (s *Service) directoryManager() localfs.DirectoryManager {
	if s.Directories == nil {
		return localfs.NewDirectoryManager()
	}
	return s.Directories
}

func chooseOperation(primary metadata.Operation, fallback metadata.Operation) metadata.Operation {
	if primary.ID == "" {
		return fallback
	}
	return primary
}

func chooseContainer(primary metadata.Container, fallback metadata.Container) metadata.Container {
	if primary.ID == "" {
		return fallback
	}
	return primary
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZeroCode(values ...metadata.ErrorCode) metadata.ErrorCode {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
