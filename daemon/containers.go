package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	daemonconfig "github.com/donglin-wang/chamber/daemon/config"
	"github.com/donglin-wang/chamber/daemon/metadata"
	chbundle "github.com/donglin-wang/chamber/internal/bundle"
	chruntime "github.com/donglin-wang/chamber/internal/runtime"
	"github.com/google/uuid"
)

type runContainerRequest struct {
	Image   string   `json:"image"`
	Command []string `json:"command"`
}

type runContainerResponse struct {
	OperationID string                  `json:"operation_id"`
	ID          string                  `json:"id"`
	ImageDigest string                  `json:"image_digest"`
	State       metadata.ContainerState `json:"state"`
}

type listContainersResponse struct {
	Containers []containerResponse `json:"containers"`
}

type containerResponse struct {
	ID          string                  `json:"id"`
	OperationID string                  `json:"operation_id"`
	Image       string                  `json:"image"`
	ImageDigest string                  `json:"image_digest"`
	Runtime     string                  `json:"runtime"`
	State       metadata.ContainerState `json:"state"`
	CreatedAt   time.Time               `json:"created_at"`
	UpdatedAt   time.Time               `json:"updated_at"`
	ExitCode    *int                    `json:"exit_code,omitempty"`
	ErrorCode   metadata.ErrorCode      `json:"error_code,omitempty"`
}

func registerContainerRoutes(
	mux *http.ServeMux,
	cfg daemonconfig.Config,
	store metadata.Store,
	runtime chruntime.Runtime,
	provisioner chbundle.Provisioner,
	lifetime context.Context,
) {
	runtimeCtx := lifetime
	if runtimeCtx == nil {
		runtimeCtx = context.Background()
	}

	mux.HandleFunc("GET /v1/containers", func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeDaemonError(w, fmt.Errorf("metadata store is required"))
			return
		}

		containers, err := store.ListContainers(r.Context())
		if err != nil {
			writeDaemonError(w, operationError("", metadata.ErrMetadataFailed, err))
			return
		}

		response := listContainersResponse{
			Containers: make([]containerResponse, 0, len(containers)),
		}
		for _, container := range containers {
			response.Containers = append(response.Containers, newContainerResponse(container))
		}
		writeJSON(w, http.StatusOK, response)
	})

	mux.HandleFunc("POST /v1/containers/run", func(w http.ResponseWriter, r *http.Request) {
		var request runContainerRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON object with image and command")
			return
		}

		if strings.TrimSpace(request.Image) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "image is required")
			return
		}
		if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "command is required")
			return
		}

		result, err := runContainer(
			r.Context(),
			cfg,
			store,
			runtime,
			provisioner,
			runtimeCtx,
			strings.TrimSpace(request.Image),
			request.Command,
		)
		if err != nil {
			writeDaemonError(w, err)
			return
		}

		writeOperationJSON(w, http.StatusCreated, result.operation.ID, runContainerResponse{
			OperationID: result.operation.ID,
			ID:          result.container.ID,
			ImageDigest: result.container.ImageDigest,
			State:       result.container.State,
		})
	})

	mux.HandleFunc("GET /v1/containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		containerID := strings.TrimSpace(r.PathValue("id"))
		if containerID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "container id is required")
			return
		}

		stream := strings.TrimSpace(r.URL.Query().Get("stream"))
		if stream == "" {
			stream = chruntime.StdoutLogStream
		}
		if stream != chruntime.StdoutLogStream && stream != chruntime.StderrLogStream {
			writeError(w, http.StatusBadRequest, "invalid_request", "unsupported log stream")
			return
		}

		if store == nil {
			writeDaemonError(w, fmt.Errorf("metadata store is required"))
			return
		}
		container, err := store.GetContainer(r.Context(), containerID)
		if errors.Is(err, metadata.ErrNotFound) {
			writeDaemonError(w, operationError("", metadata.ErrContainerNotFound, err))
			return
		}
		if err != nil {
			writeDaemonError(w, operationError("", metadata.ErrMetadataFailed, err))
			return
		}
		if runtime == nil {
			writeDaemonError(w, fmt.Errorf("runtime is required"))
			return
		}
		content, err := runtime.ReadLog(container.ID, stream)
		if errors.Is(err, os.ErrNotExist) {
			writeDaemonError(w, operationError("", metadata.ErrLogNotFound, err))
			return
		}
		if err != nil {
			writeDaemonError(w, operationError("", metadata.ErrMetadataFailed, err))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Chamber-Container-ID", container.ID)
		w.Header().Set("X-Chamber-Log-Stream", stream)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	})
}

type runContainerResult struct {
	operation metadata.Operation
	container metadata.Container
}

func runContainer(
	ctx context.Context,
	cfg daemonconfig.Config,
	store metadata.Store,
	runtime chruntime.Runtime,
	provisioner chbundle.Provisioner,
	runtimeCtx context.Context,
	imageRef string,
	command []string,
) (runContainerResult, error) {
	if store == nil {
		return runContainerResult{}, fmt.Errorf("metadata store is required")
	}
	if provisioner == nil {
		return runContainerResult{}, fmt.Errorf("bundle provisioner is required")
	}
	if runtime == nil {
		return runContainerResult{}, fmt.Errorf("runtime is required")
	}
	if runtimeCtx == nil {
		runtimeCtx = context.Background()
	}

	startedAt := time.Now().UTC()
	operationUUID, err := uuid.NewV7()
	if err != nil {
		return runContainerResult{}, fmt.Errorf("generate run operation id: %w", err)
	}
	containerUUID, err := uuid.NewV7()
	if err != nil {
		return runContainerResult{}, fmt.Errorf("generate container id: %w", err)
	}
	operationID := operationUUID.String()
	containerID := containerUUID.String()
	operation := metadata.Operation{
		ID:         operationID,
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: containerID,
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := store.CreateOperation(ctx, operation); err != nil {
		return runContainerResult{}, fmt.Errorf("create run operation: %w", err)
	}

	image, err := store.GetImage(ctx, imageRef)
	if err != nil {
		code := metadata.ErrMetadataFailed
		if errors.Is(err, metadata.ErrNotFound) {
			code = metadata.ErrImageNotFound
		}
		_, transitionErr := store.FailOperation(ctx, operationID, code)
		failErr := operationError(operationID, code, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	runtimeName := cfg.Runtime.Name
	if runtimeName == "" {
		runtimeName = chruntime.DefaultName
	}

	provisioned, err := provisioner.Provision(ctx, chbundle.ProvisionRequest{
		ContainerID: containerID,
		ImageLayout: image.LayoutPath,
		ImageRef:    image.Reference,
		Command:     command,
	})
	if err != nil {
		failedOperation, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrBundlePrepareFailed)
		failErr := operationError(operationID, metadata.ErrBundlePrepareFailed, errors.Join(err, transitionErr))
		if failedOperation.ID != "" {
			operation = failedOperation
		}
		return runContainerResult{operation: operation}, failErr
	}
	if strings.TrimSpace(provisioned.BundlePath) == "" {
		err := fmt.Errorf("bundle provisioner returned empty bundle path")
		failedOperation, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrBundlePrepareFailed)
		failErr := operationError(operationID, metadata.ErrBundlePrepareFailed, errors.Join(err, transitionErr))
		if failedOperation.ID != "" {
			operation = failedOperation
		}
		return runContainerResult{operation: operation}, failErr
	}

	now := time.Now().UTC()
	container := metadata.Container{
		ID:          containerID,
		OperationID: operationID,
		ImageDigest: image.Digest,
		ImageRef:    image.Reference,
		BundlePath:  provisioned.BundlePath,
		Runtime:     runtimeName,
		State:       metadata.ContainerCreating,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.CreateContainer(ctx, container); err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrMetadataFailed)
		failErr := operationError(operationID, metadata.ErrMetadataFailed, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	starting, err := store.TransitionContainer(ctx, containerID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State: metadata.ContainerStarting,
		At:    time.Now().UTC(),
	})
	if err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrMetadataFailed)
		failErr := operationError(operationID, metadata.ErrMetadataFailed, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation, container: container}, failErr
	}

	start, err := runtime.Run(runtimeCtx, chruntime.RunRequest{
		Bundle: provisioned,
	})
	if err != nil {
		failedContainer, failedOperation, transitionErr := store.FailContainerAndOperation(ctx, containerID, metadata.ContainerStarting, operationID, metadata.ErrRuntimeStartFailed)
		failErr := operationError(operationID, metadata.ErrRuntimeStartFailed, errors.Join(err, transitionErr))
		if failedOperation.ID != "" {
			operation = failedOperation
		}
		if failedContainer.ID != "" {
			starting = failedContainer
		}
		return runContainerResult{
			operation: operation,
			container: starting,
		}, failErr
	}

	switch start.State {
	case chruntime.ProcessRunning:
		running, err := store.TransitionContainer(ctx, containerID, metadata.ContainerStarting, metadata.ContainerUpdate{
			State: metadata.ContainerRunning,
			At:    time.Now().UTC(),
		})
		if err != nil {
			go finishRunningProcess(store, operationID, containerID, metadata.ContainerStarting, start.Process)
			return runContainerResult{operation: operation, container: starting}, operationError(operationID, metadata.ErrMetadataFailed, err)
		}
		go finishRunningProcess(store, operationID, containerID, metadata.ContainerRunning, start.Process)
		return runContainerResult{operation: operation, container: running}, nil
	case chruntime.ProcessExited:
		exited, completed, err := finishExitedProcess(ctx, store, operationID, containerID, metadata.ContainerStarting, start.Process)
		return runContainerResult{
			operation: completed,
			container: exited,
		}, err
	default:
		err := fmt.Errorf("runtime returned unknown observed state %q", start.State)
		failedContainer, failedOperation, transitionErr := store.FailContainerAndOperation(ctx, containerID, metadata.ContainerStarting, operationID, metadata.ErrRuntimeStartFailed)
		failErr := operationError(operationID, metadata.ErrRuntimeStartFailed, errors.Join(err, transitionErr))
		if failedOperation.ID != "" {
			operation = failedOperation
		}
		if failedContainer.ID != "" {
			starting = failedContainer
		}
		return runContainerResult{
			operation: operation,
			container: starting,
		}, failErr
	}
}

func finishRunningProcess(store metadata.Store, operationID string, containerID string, from metadata.ContainerState, process chruntime.Process) {
	_, _, err := finishExitedProcess(context.Background(), store, operationID, containerID, from, process)
	if err != nil {
		// The HTTP request already returned. Leave the error in logs; the
		// operation/container records remain the durable debugging surface.
		fmt.Fprintf(os.Stderr, "record container exit: operation=%s container=%s error=%v\n", operationID, containerID, err)
	}
}

func finishExitedProcess(ctx context.Context, store metadata.Store, operationID string, containerID string, from metadata.ContainerState, process chruntime.Process) (metadata.Container, metadata.Operation, error) {
	if process == nil {
		err := fmt.Errorf("runtime process is required")
		failedContainer, failedOperation, transitionErr := store.FailContainerAndOperation(ctx, containerID, from, operationID, metadata.ErrRuntimeWaitFailed)
		failErr := operationError(operationID, metadata.ErrRuntimeWaitFailed, errors.Join(err, transitionErr))
		return failedContainer, failedOperation, failErr
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

	exited, containerErr := store.TransitionContainer(ctx, containerID, from, metadata.ContainerUpdate{
		State:     metadata.ContainerExited,
		At:        time.Now().UTC(),
		ExitCode:  exitCodePtr,
		ErrorCode: string(code),
	})
	if containerErr != nil {
		return exited, metadata.Operation{}, operationError(operationID, metadata.ErrMetadataFailed, errors.Join(waitErr, containerErr))
	}

	var operation metadata.Operation
	var operationErr error
	if operationState == metadata.OperationSucceeded {
		operation, operationErr = store.SucceedOperation(ctx, operationID)
	} else {
		operation, operationErr = store.FailOperation(ctx, operationID, code)
	}
	if operationErr != nil {
		errorCode := code
		if errorCode == "" {
			errorCode = metadata.ErrMetadataFailed
		}
		return exited, operation, operationError(operationID, errorCode, errors.Join(waitErr, operationErr))
	}
	if waitErr != nil {
		return exited, operation, operationError(operationID, code, waitErr)
	}
	if exitCode != 0 {
		return exited, operation, operationError(operationID, code, fmt.Errorf("container exited with status %d", exitCode))
	}
	return exited, operation, nil
}

func newContainerResponse(container metadata.Container) containerResponse {
	return containerResponse{
		ID:          container.ID,
		OperationID: container.OperationID,
		Image:       container.ImageRef,
		ImageDigest: container.ImageDigest,
		Runtime:     container.Runtime,
		State:       container.State,
		CreatedAt:   container.CreatedAt,
		UpdatedAt:   container.UpdatedAt,
		ExitCode:    container.ExitCode,
		ErrorCode:   container.ErrorCode,
	}
}
