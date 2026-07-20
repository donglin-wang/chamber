package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
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
	ErrorCode   chamberErrors.Code      `json:"error_code,omitempty"`
}

func registerContainerRoutes(
	mux *http.ServeMux,
	store metadata.Store,
	runtime chamberRuntimeShared.Runtime,
	provisioner chamberBundleShared.Provisioner,
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
			writeDaemonError(w, operationError("", chamberErrors.ErrMetadataFailed, err))
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
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "request body must be a JSON object with image and command")
			return
		}

		if strings.TrimSpace(request.Image) == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "image is required")
			return
		}
		if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "command is required")
			return
		}

		result, err := runContainer(
			r.Context(),
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
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "container id is required")
			return
		}

		rawStream := strings.TrimSpace(r.URL.Query().Get("stream"))
		if rawStream == "" {
			rawStream = string(chamberRuntimeShared.StdoutLogStream)
		}
		stream := chamberRuntimeShared.LogStream(rawStream)
		if stream != chamberRuntimeShared.StdoutLogStream && stream != chamberRuntimeShared.StderrLogStream {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "unsupported log stream")
			return
		}

		if store == nil {
			writeDaemonError(w, fmt.Errorf("metadata store is required"))
			return
		}
		container, err := store.GetContainer(r.Context(), containerID)
		if errors.Is(err, metadata.ErrNotFound) {
			writeDaemonError(w, operationError("", chamberErrors.ErrContainerNotFound, err))
			return
		}
		if err != nil {
			writeDaemonError(w, operationError("", chamberErrors.ErrMetadataFailed, err))
			return
		}
		if runtime == nil {
			writeDaemonError(w, fmt.Errorf("runtime is required"))
			return
		}
		content, err := runtime.ReadLog(container.ID, stream)
		if errors.Is(err, os.ErrNotExist) {
			writeDaemonError(w, operationError("", chamberErrors.ErrLogNotFound, err))
			return
		}
		if err != nil {
			writeDaemonError(w, operationError("", chamberErrors.ErrMetadataFailed, err))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Chamber-Container-ID", container.ID)
		w.Header().Set("X-Chamber-Log-Stream", string(stream))
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
	store metadata.Store,
	runtime chamberRuntimeShared.Runtime,
	provisioner chamberBundleShared.Provisioner,
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
		code := chamberErrors.ErrMetadataFailed
		if errors.Is(err, metadata.ErrNotFound) {
			code = chamberErrors.ErrImageNotFound
		}
		_, transitionErr := store.FailOperation(ctx, operationID, code)
		failErr := operationError(operationID, code, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	runtimeName := runtime.Descriptor().Name
	if runtimeName == "" {
		runtimeName = runtime.Binary().Name
	}

	provisioned, err := provisioner.Provision(ctx, chamberBundleShared.ProvisionRequest{
		ContainerID: containerID,
		ImageLayout: image.LayoutPath,
		ImageRef:    image.Reference,
		Process: chamberBundleShared.ProcessSpec{
			Args: command,
		},
	})
	if err != nil {
		failedOperation, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrBundlePrepareFailed)
		failErr := operationError(operationID, chamberErrors.ErrBundlePrepareFailed, errors.Join(err, transitionErr))
		if failedOperation.ID != "" {
			operation = failedOperation
		}
		return runContainerResult{operation: operation}, failErr
	}
	if strings.TrimSpace(provisioned.BundlePath) == "" {
		err := fmt.Errorf("bundle provisioner returned empty bundle path")
		failedOperation, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrBundlePrepareFailed)
		failErr := operationError(operationID, chamberErrors.ErrBundlePrepareFailed, errors.Join(err, transitionErr))
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
		_, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrMetadataFailed)
		failErr := operationError(operationID, chamberErrors.ErrMetadataFailed, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	starting, err := store.TransitionContainer(ctx, containerID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State: metadata.ContainerStarting,
		At:    time.Now().UTC(),
	})
	if err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrMetadataFailed)
		failErr := operationError(operationID, chamberErrors.ErrMetadataFailed, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation, container: container}, failErr
	}

	process, err := runtime.Run(runtimeCtx, chamberRuntimeShared.RunRequest{
		Bundle: provisioned,
	})
	if err != nil {
		failedContainer, failedOperation, transitionErr := store.FailContainerAndOperation(ctx, containerID, metadata.ContainerStarting, operationID, chamberErrors.ErrRuntimeStartFailed)
		failErr := operationError(operationID, chamberErrors.ErrRuntimeStartFailed, errors.Join(err, transitionErr))
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

	running, err := store.TransitionContainer(ctx, containerID, metadata.ContainerStarting, metadata.ContainerUpdate{
		State: metadata.ContainerRunning,
		At:    time.Now().UTC(),
	})
	if err != nil {
		go finishRunningProcess(store, operationID, containerID, metadata.ContainerStarting, process)
		return runContainerResult{operation: operation, container: starting}, operationError(operationID, chamberErrors.ErrMetadataFailed, err)
	}
	go finishRunningProcess(store, operationID, containerID, metadata.ContainerRunning, process)
	return runContainerResult{operation: operation, container: running}, nil
}

func finishRunningProcess(store metadata.Store, operationID string, containerID string, from metadata.ContainerState, process chamberRuntimeShared.Process) {
	_, _, err := finishExitedProcess(context.Background(), store, operationID, containerID, from, process)
	if err != nil {
		// The HTTP request already returned. Leave the error in logs; the
		// operation/container records remain the durable debugging surface.
		fmt.Fprintf(os.Stderr, "record container exit: operation=%s container=%s error=%v\n", operationID, containerID, err)
	}
}

func finishExitedProcess(ctx context.Context, store metadata.Store, operationID string, containerID string, from metadata.ContainerState, process chamberRuntimeShared.Process) (metadata.Container, metadata.Operation, error) {
	if process == nil {
		err := fmt.Errorf("runtime process is required")
		failedContainer, failedOperation, transitionErr := store.FailContainerAndOperation(ctx, containerID, from, operationID, chamberErrors.ErrRuntimeWaitFailed)
		failErr := operationError(operationID, chamberErrors.ErrRuntimeWaitFailed, errors.Join(err, transitionErr))
		return failedContainer, failedOperation, failErr
	}

	exitCode, waitErr := process.Wait()
	exitCodePtr := &exitCode
	code := chamberErrors.Code("")
	operationState := metadata.OperationSucceeded
	if waitErr != nil {
		code = chamberErrors.ErrRuntimeWaitFailed
		operationState = metadata.OperationFailed
	} else if exitCode != 0 {
		code = chamberErrors.ErrContainerExitNonzero
		operationState = metadata.OperationFailed
	}

	exited, containerErr := store.TransitionContainer(ctx, containerID, from, metadata.ContainerUpdate{
		State:     metadata.ContainerExited,
		At:        time.Now().UTC(),
		ExitCode:  exitCodePtr,
		ErrorCode: code,
	})
	if containerErr != nil {
		return exited, metadata.Operation{}, operationError(operationID, chamberErrors.ErrMetadataFailed, errors.Join(waitErr, containerErr))
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
			errorCode = chamberErrors.ErrMetadataFailed
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
