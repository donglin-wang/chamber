package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/google/uuid"
)

type runContainerRequest struct {
	Image   string                `json:"image"`
	Command []string              `json:"command"`
	Mounts  []chamberBundle.Mount `json:"mounts,omitempty"`
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
	imageStore chamberImage.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	supervisorRoot string,
	startSupervisor startSupervisorFunc,
) {
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
			imageStore,
			runtimeConfig,
			provisioner,
			supervisorRoot,
			startSupervisor,
			strings.TrimSpace(request.Image),
			request.Command,
			request.Mounts,
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
			rawStream = string(chamberRuntime.StdoutLogStream)
		}
		stream := chamberRuntime.LogStream(rawStream)
		if stream != chamberRuntime.StdoutLogStream && stream != chamberRuntime.StderrLogStream {
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
		logPath := container.StdoutPath
		if stream == chamberRuntime.StderrLogStream {
			logPath = container.StderrPath
		}
		if logPath == "" {
			writeDaemonError(w, operationError("", chamberErrors.ErrLogNotFound, os.ErrNotExist))
			return
		}
		content, err := os.ReadFile(logPath)
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
	imageStore chamberImage.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	supervisorRoot string,
	startSupervisor startSupervisorFunc,
	imageRef string,
	command []string,
	mounts []chamberBundle.Mount,
) (runContainerResult, error) {
	if store == nil {
		return runContainerResult{}, fmt.Errorf("metadata store is required")
	}
	if provisioner == nil {
		return runContainerResult{}, fmt.Errorf("bundle provisioner is required")
	}
	if imageStore == nil {
		return runContainerResult{}, fmt.Errorf("image store is required")
	}
	if strings.TrimSpace(runtimeConfig.Name) == "" {
		return runContainerResult{}, fmt.Errorf("%w: runtime name is required", chamberErrors.ErrInvalidRequest)
	}
	if strings.TrimSpace(supervisorRoot) == "" {
		return runContainerResult{}, fmt.Errorf("%w: supervisor root is required", chamberErrors.ErrInvalidRequest)
	}
	if startSupervisor == nil {
		return runContainerResult{}, fmt.Errorf("supervisor starter is required")
	}
	canonicalImageRef, err := chamberImage.CanonicalImageReference(imageRef)
	if err != nil {
		return runContainerResult{}, err
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

	image, err := store.GetImage(ctx, canonicalImageRef)
	if err != nil {
		code := chamberErrors.ErrMetadataFailed
		if errors.Is(err, metadata.ErrNotFound) {
			code = chamberErrors.ErrImageNotFound
		}
		_, transitionErr := store.FailOperation(ctx, operationID, code)
		failErr := operationError(operationID, code, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	runtimeName := runtimeConfig.Name
	terminal := false
	imageLayout, err := imageStore.Layout(ctx)
	if err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrInvalidImageLayout)
		failErr := operationError(operationID, chamberErrors.ErrInvalidImageLayout, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	provisioned, err := provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
		ContainerID:   containerID,
		ImageLayout:   imageLayout,
		ImageRef:      image.Reference,
		ImageDigest:   image.Digest,
		ImagePlatform: image.Platform,
		Process: chamberBundle.ProcessSpec{
			Args:     command,
			Terminal: &terminal,
		},
		Mounts: mounts,
	})
	if err != nil {
		code := chamberErrors.CodeFromError(err, chamberErrors.ErrBundlePrepareFailed)
		failedOperation, transitionErr := store.FailOperation(ctx, operationID, code)
		failErr := operationError(operationID, code, errors.Join(err, transitionErr))
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

	supervisorDir := filepath.Join(supervisorRoot, containerID)
	supervisorPath := filepath.Join(supervisorDir, "supervisor.json")
	stdoutPath := filepath.Join(supervisorDir, "stdout.log")
	stderrPath := filepath.Join(supervisorDir, "stderr.log")
	now := time.Now().UTC()
	container := metadata.Container{
		ID:             containerID,
		OperationID:    operationID,
		ImageDigest:    image.Digest,
		ImageRef:       image.Reference,
		BundlePath:     provisioned.BundlePath,
		StdoutPath:     stdoutPath,
		StderrPath:     stderrPath,
		Runtime:        runtimeName,
		RuntimeRoot:    runtimeConfig.RuntimeRoot,
		SupervisorPath: supervisorPath,
		State:          metadata.ContainerCreating,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := store.CreateContainer(ctx, container); err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrMetadataFailed)
		failErr := operationError(operationID, chamberErrors.ErrMetadataFailed, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	supervisorState := supervisorFile{
		Version:     supervisorFileVersion,
		Phase:       supervisorPrepared,
		OperationID: operationID,
		ContainerID: containerID,
		Runtime:     runtimeConfig,
		Bundle:      provisioned,
		StdoutPath:  stdoutPath,
		StderrPath:  stderrPath,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := writeSupervisorFile(supervisorPath, supervisorState); err != nil {
		failedContainer, failedOperation, transitionErr := store.FailContainerAndOperation(ctx, containerID, metadata.ContainerCreating, operationID, chamberErrors.ErrFilesystemFailed)
		failErr := operationError(operationID, chamberErrors.ErrFilesystemFailed, errors.Join(err, transitionErr))
		if failedOperation.ID != "" {
			operation = failedOperation
		}
		if failedContainer.ID != "" {
			container = failedContainer
		}
		return runContainerResult{operation: operation, container: container}, failErr
	}

	supervisorPID, waitSupervisor, err := startSupervisor(ctx, supervisorPath)
	if err != nil {
		code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeStartFailed)
		failedContainer, failedOperation, transitionErr := store.FailContainerAndOperation(ctx, containerID, metadata.ContainerCreating, operationID, code)
		failErr := operationError(operationID, code, errors.Join(err, transitionErr))
		if failedOperation.ID != "" {
			operation = failedOperation
		}
		if failedContainer.ID != "" {
			container = failedContainer
		}
		return runContainerResult{operation: operation, container: container}, failErr
	}

	starting, err := store.TransitionContainer(ctx, containerID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State:          metadata.ContainerStarting,
		At:             time.Now().UTC(),
		StdoutPath:     stdoutPath,
		StderrPath:     stderrPath,
		RuntimeRoot:    runtimeConfig.RuntimeRoot,
		SupervisorPath: supervisorPath,
		SupervisorPID:  supervisorPID,
	})
	if err != nil {
		go monitorSupervisor(context.Background(), store, operationID, containerID, supervisorPath, waitSupervisor)
		_, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrMetadataFailed)
		failErr := operationError(operationID, chamberErrors.ErrMetadataFailed, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation, container: container}, failErr
	}

	go monitorSupervisor(context.Background(), store, operationID, containerID, supervisorPath, waitSupervisor)
	return runContainerResult{operation: operation, container: starting}, nil
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
