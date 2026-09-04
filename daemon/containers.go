package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
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

type openContainerFunc func(ctx context.Context, config chamberRuntime.Config, containerID string) (chamberRuntime.ContainerHandle, error)

type terminateSupervisorFunc func(pid int, startTime uint64) error

const (
	daemonLifecyclePauseDirEnv   = "CHAMBER_TEST_DAEMON_LIFECYCLE_PAUSE_DIR"
	daemonLifecyclePausePointEnv = "CHAMBER_TEST_DAEMON_LIFECYCLE_PAUSE_POINT"

	daemonPauseAfterCreateAdmission = "after-create-admission"
	daemonPauseAfterStartAdmission  = "after-start-admission"
	daemonPauseAfterPrepared        = "after-prepared"
)

func registerContainerRoutes(
	mux *http.ServeMux,
	store metadata.Store,
	imageStore chamberImage.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	supervisorRoot string,
	startSupervisor startSupervisorFunc,
	openContainer openContainerFunc,
	terminateSupervisor terminateSupervisorFunc,
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

	mux.HandleFunc("GET /v1/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		containerID := strings.TrimSpace(r.PathValue("id"))
		if containerID == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "container id is required")
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
		writeJSON(w, http.StatusOK, newContainerResponse(container))
	})

	mux.HandleFunc("POST /v1/containers/create", func(w http.ResponseWriter, r *http.Request) {
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
		lockKey, canonicalImage, err := canonicalImageOperationLockKey(request.Image)
		if err != nil {
			writeDaemonError(w, err)
			return
		}

		operationCtx := context.WithoutCancel(r.Context())
		var result createContainerResult
		err = nil
		daemonOperationLocks.with(lockKey, func() {
			result, err = createContainer(
				operationCtx,
				store,
				imageStore,
				runtimeConfig,
				provisioner,
				supervisorRoot,
				canonicalImage,
				request.Command,
				request.Mounts,
			)
		})
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
		lockKey, canonicalImage, err := canonicalImageOperationLockKey(request.Image)
		if err != nil {
			writeDaemonError(w, err)
			return
		}

		operationCtx := context.WithoutCancel(r.Context())
		var result runContainerResult
		err = nil
		daemonOperationLocks.with(lockKey, func() {
			result, err = runContainer(
				operationCtx,
				store,
				imageStore,
				runtimeConfig,
				provisioner,
				supervisorRoot,
				startSupervisor,
				terminateSupervisor,
				canonicalImage,
				request.Command,
				request.Mounts,
			)
		})
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

	mux.HandleFunc("POST /v1/containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		containerID := strings.TrimSpace(r.PathValue("id"))
		if containerID == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "container id is required")
			return
		}
		operationCtx := context.WithoutCancel(r.Context())
		var result startContainerResult
		var err error
		daemonOperationLocks.with("container:"+containerID, func() {
			result, err = startContainer(
				operationCtx,
				store,
				startSupervisor,
				terminateSupervisor,
				containerID,
			)
		})
		if err != nil {
			writeDaemonError(w, err)
			return
		}
		writeOperationJSON(w, http.StatusOK, result.operation.ID, newContainerResponse(result.container))
	})

	mux.HandleFunc("POST /v1/containers/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		containerID := strings.TrimSpace(r.PathValue("id"))
		if containerID == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "container id is required")
			return
		}
		operationCtx := context.WithoutCancel(r.Context())
		var result cancelContainerResult
		var err error
		daemonOperationLocks.with("container:"+containerID, func() {
			result, err = cancelContainer(
				operationCtx,
				store,
				runtimeConfig,
				provisioner,
				openContainer,
				terminateSupervisor,
				containerID,
			)
		})
		if err != nil {
			writeDaemonError(w, err)
			return
		}
		writeOperationJSON(w, http.StatusOK, result.operation.ID, newContainerResponse(result.container))
	})

	mux.HandleFunc("POST /v1/containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		containerID := strings.TrimSpace(r.PathValue("id"))
		if containerID == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "container id is required")
			return
		}
		operationCtx := context.WithoutCancel(r.Context())
		var result stopContainerResult
		var err error
		daemonOperationLocks.with("container:"+containerID, func() {
			result, err = stopContainer(
				operationCtx,
				store,
				runtimeConfig,
				openContainer,
				containerID,
			)
		})
		if err != nil {
			writeDaemonError(w, err)
			return
		}
		writeOperationJSON(w, http.StatusOK, result.operation.ID, newContainerResponse(result.container))
	})

	mux.HandleFunc("DELETE /v1/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		containerID := strings.TrimSpace(r.PathValue("id"))
		if containerID == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "container id is required")
			return
		}
		operationCtx := context.WithoutCancel(r.Context())
		var result removeContainerResult
		var err error
		daemonOperationLocks.with("container:"+containerID, func() {
			result, err = removeContainer(
				operationCtx,
				store,
				runtimeConfig,
				provisioner,
				openContainer,
				terminateSupervisor,
				containerID,
			)
		})
		if err != nil {
			writeDaemonError(w, err)
			return
		}
		writeOperationJSON(w, http.StatusOK, result.operation.ID, newContainerResponse(result.container))
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

func openRuntimeContainer(ctx context.Context, config chamberRuntime.Config, containerID string) (chamberRuntime.ContainerHandle, error) {
	rt, err := chamberRuntimeFactory.NewRuntime(ctx, config)
	if err != nil {
		return nil, err
	}
	return rt.Open(ctx, containerID)
}

func terminateSupervisorProcessGroup(pid int, startTime uint64) error {
	if pid <= 0 || startTime == 0 {
		return nil
	}
	matches, err := supervisorProcessMatches(pid, startTime)
	if err != nil {
		return err
	}
	if !matches {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("%w: terminate supervisor process group %d: %w", chamberErrors.ErrRuntimeControlFailed, pid, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		matches, err = supervisorProcessMatches(pid, startTime)
		if err != nil || !matches {
			return err
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("%w: kill supervisor process group %d: %w", chamberErrors.ErrRuntimeControlFailed, pid, err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		matches, err = supervisorProcessMatches(pid, startTime)
		if err != nil || !matches {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: supervisor process %d remained alive after forced termination", chamberErrors.ErrRuntimeControlFailed, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func removeContainer(
	ctx context.Context,
	store metadata.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	openContainer openContainerFunc,
	terminateSupervisor terminateSupervisorFunc,
	containerID string,
) (removeContainerResult, error) {
	if store == nil {
		return removeContainerResult{}, fmt.Errorf("metadata store is required")
	}
	cleanup, operation, err := admitContainerCleanup(ctx, store, containerID, metadata.RemoveOperation, true)
	if err != nil {
		return removeContainerResult{}, err
	}
	container, operation, err := executeContainerCleanup(ctx, store, runtimeConfig, provisioner, openContainer, terminateSupervisor, cleanup)
	return removeContainerResult{operation: operation, container: container}, err
}

type removeContainerResult struct {
	operation metadata.Operation
	container metadata.Container
}

type stopContainerResult struct {
	operation metadata.Operation
	container metadata.Container
}

func stopContainer(
	ctx context.Context,
	store metadata.Store,
	runtimeConfig chamberRuntime.Config,
	openContainer openContainerFunc,
	containerID string,
) (stopContainerResult, error) {
	if store == nil {
		return stopContainerResult{}, fmt.Errorf("metadata store is required")
	}
	if openContainer == nil {
		return stopContainerResult{}, fmt.Errorf("runtime opener is required")
	}

	container, err := store.GetContainer(ctx, containerID)
	if errors.Is(err, metadata.ErrNotFound) {
		return stopContainerResult{}, operationError("", chamberErrors.ErrContainerNotFound, err)
	}
	if err != nil {
		return stopContainerResult{}, operationError("", chamberErrors.ErrMetadataFailed, err)
	}

	startedAt := time.Now().UTC()
	operationUUID, err := uuid.NewV7()
	if err != nil {
		return stopContainerResult{}, fmt.Errorf("generate stop operation id: %w", err)
	}
	operationID := operationUUID.String()
	operation := metadata.Operation{
		ID:         operationID,
		Kind:       metadata.StopOperation,
		State:      metadata.OperationRunning,
		ResourceID: container.ID,
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := store.CreateOperation(ctx, operation); err != nil {
		return stopContainerResult{}, operationError("", chamberErrors.ErrMetadataFailed, err)
	}

	if container.State == metadata.ContainerExited || container.State == metadata.ContainerFailed {
		completed, err := store.SucceedOperation(ctx, operationID)
		if err != nil {
			return stopContainerResult{operation: operation, container: container}, operationError(operationID, chamberErrors.ErrMetadataFailed, err)
		}
		return stopContainerResult{operation: completed, container: container}, nil
	}
	if container.State != metadata.ContainerStarting && container.State != metadata.ContainerRunning {
		err := fmt.Errorf("%w: cannot stop container %q while state is %q", chamberErrors.ErrStateConflict, container.ID, container.State)
		failed, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrStateConflict)
		if transitionErr == nil {
			operation = failed
		}
		return stopContainerResult{operation: operation, container: container}, operationError(operationID, chamberErrors.ErrStateConflict, errors.Join(err, transitionErr))
	}

	controlConfig := runtimeConfig
	if strings.TrimSpace(container.RuntimeRoot) != "" {
		controlConfig.RuntimeRoot = container.RuntimeRoot
	}
	handle, err := openContainer(ctx, controlConfig, container.ID)
	if err != nil {
		code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed)
		failed, transitionErr := store.FailOperation(ctx, operationID, code)
		if transitionErr == nil {
			operation = failed
		}
		return stopContainerResult{operation: operation, container: container}, operationError(operationID, code, errors.Join(err, transitionErr))
	}
	if err := handle.Signal(ctx, syscall.SIGTERM); err != nil {
		code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed)
		failed, transitionErr := store.FailOperation(ctx, operationID, code)
		if transitionErr == nil {
			operation = failed
		}
		return stopContainerResult{operation: operation, container: container}, operationError(operationID, code, errors.Join(err, transitionErr))
	}

	completed, err := store.SucceedOperation(ctx, operationID)
	if err != nil {
		return stopContainerResult{operation: operation, container: container}, operationError(operationID, chamberErrors.ErrMetadataFailed, err)
	}
	return stopContainerResult{operation: completed, container: container}, nil
}

func deleteContainerArtifacts(
	ctx context.Context,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	openContainer openContainerFunc,
	terminateSupervisor terminateSupervisorFunc,
	container metadata.Container,
	removeSupervisorEvidence bool,
) error {
	if provisioner == nil {
		return fmt.Errorf("bundle provisioner is required")
	}
	if openContainer == nil {
		return fmt.Errorf("runtime opener is required")
	}
	if terminateSupervisor != nil {
		if err := terminateSupervisor(container.SupervisorPID, container.SupervisorStartTime); err != nil {
			code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed)
			return operationError(container.OperationID, code, err)
		}
	}
	if container.State != metadata.ContainerCreated && container.State != metadata.ContainerCreating {
		controlConfig := runtimeConfig
		if strings.TrimSpace(container.RuntimeRoot) != "" {
			controlConfig.RuntimeRoot = container.RuntimeRoot
		}
		handle, err := openContainer(ctx, controlConfig, container.ID)
		if err != nil && !looksAlreadyDeleted(err) {
			code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed)
			return operationError(container.OperationID, code, err)
		}
		if err == nil {
			if err := handle.Delete(ctx, true); err != nil && !looksAlreadyDeleted(err) {
				code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed)
				return operationError(container.OperationID, code, err)
			}
			if err := handle.DeleteLog(chamberRuntime.StdoutLogStream); err != nil && !errors.Is(err, chamberErrors.ErrLogNotFound) {
				return operationError(container.OperationID, chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed), err)
			}
			if err := handle.DeleteLog(chamberRuntime.StderrLogStream); err != nil && !errors.Is(err, chamberErrors.ErrLogNotFound) {
				return operationError(container.OperationID, chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed), err)
			}
		}
	}
	if err := provisioner.Remove(ctx, chamberBundle.ProvisionedBundle{
		ContainerID: container.ID,
		BundlePath:  container.BundlePath,
	}); err != nil {
		return operationError(container.OperationID, chamberErrors.ErrBundlePrepareFailed, err)
	}
	if removeSupervisorEvidence && strings.TrimSpace(container.SupervisorPath) != "" {
		if err := os.RemoveAll(filepath.Dir(container.SupervisorPath)); err != nil {
			return operationError(container.OperationID, chamberErrors.ErrFilesystemFailed, err)
		}
	}
	return nil
}

type cancelContainerResult struct {
	operation metadata.Operation
	container metadata.Container
}

func cancelContainer(
	ctx context.Context,
	store metadata.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	openContainer openContainerFunc,
	terminateSupervisor terminateSupervisorFunc,
	containerID string,
) (cancelContainerResult, error) {
	if store == nil {
		return cancelContainerResult{}, fmt.Errorf("metadata store is required")
	}
	if provisioner == nil {
		return cancelContainerResult{}, fmt.Errorf("bundle provisioner is required")
	}
	if openContainer == nil {
		return cancelContainerResult{}, fmt.Errorf("runtime opener is required")
	}

	cleanup, operation, err := admitContainerCleanup(ctx, store, containerID, metadata.CancelOperation, true)
	if err != nil {
		return cancelContainerResult{}, err
	}
	container, operation, err := executeContainerCleanup(ctx, store, runtimeConfig, provisioner, openContainer, terminateSupervisor, cleanup)
	return cancelContainerResult{operation: operation, container: container}, err
}

func looksAlreadyDeleted(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return errors.Is(err, os.ErrNotExist) ||
		strings.Contains(message, "does not exist") ||
		strings.Contains(message, "container does not exist")
}

type runContainerResult struct {
	operation metadata.Operation
	container metadata.Container
}

type createContainerResult struct {
	operation metadata.Operation
	container metadata.Container
}

type startContainerResult struct {
	operation metadata.Operation
	container metadata.Container
}

func createContainer(
	ctx context.Context,
	store metadata.Store,
	imageStore chamberImage.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	supervisorRoot string,
	imageRef string,
	command []string,
	mounts []chamberBundle.Mount,
) (createContainerResult, error) {
	operation, containerID, err := newContainerAdmission(metadata.CreateOperation)
	if err != nil {
		return createContainerResult{}, err
	}
	releaseContainerLock := daemonOperationLocks.acquire("container:" + containerID)
	defer releaseContainerLock()
	prepared, err := prepareContainer(ctx, store, imageStore, runtimeConfig, provisioner, supervisorRoot, operation, containerID, imageRef, command, mounts)
	if err != nil {
		return createContainerResult{operation: prepared.operation, container: prepared.container}, err
	}
	completed, err := store.SucceedOperation(ctx, prepared.operation.ID)
	if err != nil {
		return createContainerResult{operation: prepared.operation, container: prepared.container}, operationError(prepared.operation.ID, chamberErrors.ErrMetadataFailed, err)
	}
	return createContainerResult{operation: completed, container: prepared.container}, nil
}

func runContainer(
	ctx context.Context,
	store metadata.Store,
	imageStore chamberImage.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	supervisorRoot string,
	startSupervisor startSupervisorFunc,
	terminateSupervisor terminateSupervisorFunc,
	imageRef string,
	command []string,
	mounts []chamberBundle.Mount,
) (runContainerResult, error) {
	operation, containerID, err := newContainerAdmission(metadata.RunOperation)
	if err != nil {
		return runContainerResult{}, err
	}
	releaseContainerLock := daemonOperationLocks.acquire("container:" + containerID)
	defer releaseContainerLock()
	prepared, err := prepareContainer(ctx, store, imageStore, runtimeConfig, provisioner, supervisorRoot, operation, containerID, imageRef, command, mounts)
	if err != nil {
		return prepared, err
	}
	starting, err := store.TransitionContainer(ctx, prepared.container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
		State: metadata.ContainerStarting,
		At:    time.Now().UTC(),
	})
	if err != nil {
		return failContainerAdmission(ctx, store, prepared.operation, prepared.container, chamberErrors.ErrMetadataFailed, err)
	}
	started, err := startPreparedContainer(ctx, store, prepared.operation, starting, startSupervisor, terminateSupervisor)
	return runContainerResult{operation: started.operation, container: started.container}, err
}

func newContainerAdmission(kind metadata.OperationKind) (metadata.Operation, string, error) {
	operationUUID, err := uuid.NewV7()
	if err != nil {
		return metadata.Operation{}, "", fmt.Errorf("generate %s operation id: %w", kind, err)
	}
	containerUUID, err := uuid.NewV7()
	if err != nil {
		return metadata.Operation{}, "", fmt.Errorf("generate container id: %w", err)
	}
	now := time.Now().UTC()
	containerID := containerUUID.String()
	return metadata.Operation{
		ID:         operationUUID.String(),
		Kind:       kind,
		State:      metadata.OperationRunning,
		ResourceID: containerID,
		StartedAt:  now,
		UpdatedAt:  now,
	}, containerID, nil
}

func startContainer(
	ctx context.Context,
	store metadata.Store,
	startSupervisor startSupervisorFunc,
	terminateSupervisor terminateSupervisorFunc,
	containerID string,
) (startContainerResult, error) {
	if store == nil {
		return startContainerResult{}, fmt.Errorf("metadata store is required")
	}
	if startSupervisor == nil {
		return startContainerResult{}, fmt.Errorf("supervisor starter is required")
	}

	container, err := store.GetContainer(ctx, containerID)
	if errors.Is(err, metadata.ErrNotFound) {
		return startContainerResult{}, operationError("", chamberErrors.ErrContainerNotFound, err)
	}
	if err != nil {
		return startContainerResult{}, operationError("", chamberErrors.ErrMetadataFailed, err)
	}
	if container.State != metadata.ContainerCreated {
		err := fmt.Errorf("%w: cannot start container %q while state is %q", chamberErrors.ErrStateConflict, container.ID, container.State)
		return startContainerResult{container: container}, operationError("", chamberErrors.ErrStateConflict, err)
	}
	startedAt := time.Now().UTC()
	operationUUID, err := uuid.NewV7()
	if err != nil {
		return startContainerResult{}, fmt.Errorf("generate start operation id: %w", err)
	}
	operation := metadata.Operation{
		ID:         operationUUID.String(),
		Kind:       metadata.StartOperation,
		State:      metadata.OperationRunning,
		ResourceID: containerID,
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	starting, err := store.CreateOperationAndTransitionContainer(ctx, operation, container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
		OperationID: operation.ID,
		State:       metadata.ContainerStarting,
		At:          startedAt,
	})
	if err != nil {
		return startContainerResult{operation: operation, container: container}, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
	}
	if err := pauseDaemonLifecyclePoint(ctx, starting.ID, operation.ID, daemonPauseAfterStartAdmission); err != nil {
		return failPreparedContainer(ctx, store, operation, starting, chamberErrors.ErrCanceled, err)
	}
	return startPreparedContainer(ctx, store, operation, starting, startSupervisor, terminateSupervisor)
}

func prepareContainer(
	ctx context.Context,
	store metadata.Store,
	imageStore chamberImage.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	supervisorRoot string,
	operation metadata.Operation,
	containerID string,
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
	if operation.Kind != metadata.CreateOperation && operation.Kind != metadata.RunOperation {
		return runContainerResult{}, fmt.Errorf("%w: unsupported prepare operation kind %q", chamberErrors.ErrInvalidRequest, operation.Kind)
	}
	canonicalImageRef, err := chamberImage.CanonicalImageReference(imageRef)
	if err != nil {
		return runContainerResult{}, err
	}

	operationID := operation.ID

	image, err := store.GetImage(ctx, canonicalImageRef)
	if err != nil {
		code := chamberErrors.ErrMetadataFailed
		if errors.Is(err, metadata.ErrNotFound) {
			code = chamberErrors.ErrImageNotFound
		}
		createErr := store.CreateOperation(ctx, operation)
		_, transitionErr := store.FailOperation(ctx, operationID, code)
		if createErr != nil {
			transitionErr = errors.Join(createErr, transitionErr)
		}
		failErr := operationError(operationID, code, errors.Join(err, transitionErr))
		return runContainerResult{operation: operation}, failErr
	}

	runtimeName := runtimeConfig.Name
	terminal := false
	imageLayout, err := imageStore.Layout(ctx)
	if err != nil {
		createErr := store.CreateOperation(ctx, operation)
		_, transitionErr := store.FailOperation(ctx, operationID, chamberErrors.ErrInvalidImageLayout)
		if createErr != nil {
			transitionErr = errors.Join(createErr, transitionErr)
		}
		failErr := operationError(operationID, chamberErrors.ErrInvalidImageLayout, errors.Join(err, transitionErr))
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
		StdoutPath:     stdoutPath,
		StderrPath:     stderrPath,
		Runtime:        runtimeName,
		RuntimeRoot:    runtimeConfig.RuntimeRoot,
		SupervisorPath: supervisorPath,
		State:          metadata.ContainerCreating,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := store.CreateContainerAndOperation(ctx, container, operation); err != nil {
		return runContainerResult{}, fmt.Errorf("create %s operation and container: %w", operation.Kind, err)
	}
	if err := pauseDaemonLifecyclePoint(ctx, container.ID, operation.ID, daemonPauseAfterCreateAdmission); err != nil {
		return failContainerAdmission(ctx, store, operation, container, chamberErrors.ErrCanceled, err)
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
		return failContainerAdmission(ctx, store, operation, container, code, err)
	}
	if strings.TrimSpace(provisioned.BundlePath) == "" {
		err := fmt.Errorf("bundle provisioner returned empty bundle path")
		return failContainerAdmission(ctx, store, operation, container, chamberErrors.ErrBundlePrepareFailed, err)
	}

	preparedAt := time.Now().UTC()
	supervisorState := supervisorFile{
		Version:     supervisorFileVersion,
		Phase:       supervisorPrepared,
		OperationID: operationID,
		ContainerID: containerID,
		Runtime:     runtimeConfig,
		Bundle:      provisioned,
		StdoutPath:  stdoutPath,
		StderrPath:  stderrPath,
		CreatedAt:   preparedAt,
		UpdatedAt:   preparedAt,
	}
	if err := writeSupervisorFile(supervisorPath, supervisorState); err != nil {
		return failContainerAdmission(ctx, store, operation, container, chamberErrors.ErrFilesystemFailed, err)
	}

	created, err := store.TransitionContainer(ctx, containerID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State:          metadata.ContainerCreated,
		At:             time.Now().UTC(),
		BundlePath:     provisioned.BundlePath,
		StdoutPath:     stdoutPath,
		StderrPath:     stderrPath,
		RuntimeRoot:    runtimeConfig.RuntimeRoot,
		SupervisorPath: supervisorPath,
	})
	if err != nil {
		return failContainerAdmission(ctx, store, operation, container, chamberErrors.ErrMetadataFailed, err)
	}
	if err := pauseDaemonLifecyclePoint(ctx, created.ID, operation.ID, daemonPauseAfterPrepared); err != nil {
		return failContainerAdmission(ctx, store, operation, created, chamberErrors.ErrCanceled, err)
	}

	return runContainerResult{operation: operation, container: created}, nil
}

func pauseDaemonLifecyclePoint(ctx context.Context, containerID string, operationID string, point string) error {
	pauseDir := strings.TrimSpace(os.Getenv(daemonLifecyclePauseDirEnv))
	if pauseDir == "" || strings.TrimSpace(os.Getenv(daemonLifecyclePausePointEnv)) != point {
		return nil
	}
	if err := os.MkdirAll(pauseDir, 0o700); err != nil {
		return fmt.Errorf("%w: create daemon lifecycle pause directory: %w", chamberErrors.ErrFilesystemFailed, err)
	}
	signalPath := filepath.Join(pauseDir, containerID+"."+point+".json")
	releasePath := filepath.Join(pauseDir, containerID+"."+point+".release")
	if err := writeSupervisorJSON(signalPath, map[string]string{
		"container_id": containerID,
		"operation_id": operationID,
		"point":        point,
	}); err != nil {
		return err
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(releasePath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: inspect daemon lifecycle pause release: %w", chamberErrors.ErrFilesystemFailed, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: daemon lifecycle pause canceled: %w", chamberErrors.ErrCanceled, ctx.Err())
		case <-ticker.C:
		}
	}
}

func failContainerAdmission(
	ctx context.Context,
	store metadata.Store,
	operation metadata.Operation,
	container metadata.Container,
	code chamberErrors.Code,
	cause error,
) (runContainerResult, error) {
	recordErr := failContainerFromCurrent(ctx, store, operation.ID, container.ID, code, cause)
	if updated, err := store.GetContainer(ctx, container.ID); err == nil {
		container = updated
	}
	if updated, err := store.GetOperation(ctx, operation.ID); err == nil {
		operation = updated
	}
	return runContainerResult{operation: operation, container: container}, operationError(operation.ID, code, errors.Join(cause, recordErr))
}

func startPreparedContainer(
	ctx context.Context,
	store metadata.Store,
	operation metadata.Operation,
	container metadata.Container,
	startSupervisor startSupervisorFunc,
	terminateSupervisor terminateSupervisorFunc,
) (startContainerResult, error) {
	if store == nil {
		return startContainerResult{}, fmt.Errorf("metadata store is required")
	}
	if startSupervisor == nil {
		return startContainerResult{}, fmt.Errorf("supervisor starter is required")
	}
	if container.State != metadata.ContainerStarting {
		err := fmt.Errorf("%w: cannot start container %q while state is %q", chamberErrors.ErrStateConflict, container.ID, container.State)
		return startContainerResult{operation: operation, container: container}, operationError(operation.ID, chamberErrors.ErrStateConflict, err)
	}

	supervisorState, err := readRequiredSupervisorFile(container.SupervisorPath)
	if err != nil {
		code := chamberErrors.CodeFromError(err, chamberErrors.ErrFilesystemFailed)
		return failPreparedContainer(ctx, store, operation, container, code, err)
	}
	supervisorState.OperationID = operation.ID
	supervisorState.Phase = supervisorPrepared
	supervisorState.RuntimeName = ""
	supervisorState.SupervisorPID = 0
	supervisorState.SupervisorStartTime = 0
	supervisorState.StartedAt = nil
	supervisorState.Result = nil
	supervisorState.UpdatedAt = time.Now().UTC()
	if err := writeSupervisorFile(container.SupervisorPath, supervisorState); err != nil {
		return failPreparedContainer(ctx, store, operation, container, chamberErrors.ErrFilesystemFailed, err)
	}

	supervisorPID, waitSupervisor, err := startSupervisor(ctx, container.SupervisorPath)
	if err != nil {
		code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeStartFailed)
		return failPreparedContainer(ctx, store, operation, container, code, err)
	}

	launchedState, present, readErr := readSupervisorFile(container.SupervisorPath, operation.ID, container.ID)
	if readErr != nil || !present {
		if terminateSupervisor != nil {
			readErr = errors.Join(readErr, terminateSupervisor(supervisorPID, launchedState.SupervisorStartTime))
		}
		if !present && readErr == nil {
			readErr = fmt.Errorf("supervisor file disappeared after launch")
		}
		return failPreparedContainer(ctx, store, operation, container, chamberErrors.ErrFilesystemFailed, readErr)
	}
	starting, err := store.SetContainerSupervisor(ctx, container.ID, metadata.ContainerStarting, supervisorPID, launchedState.SupervisorStartTime, time.Now().UTC())
	if err != nil {
		if terminateSupervisor != nil {
			err = errors.Join(err, terminateSupervisor(supervisorPID, launchedState.SupervisorStartTime))
		}
		return failPreparedContainer(ctx, store, operation, container, chamberErrors.ErrMetadataFailed, err)
	}

	go monitorSupervisor(context.Background(), store, operation.ID, container.ID, container.SupervisorPath, waitSupervisor)
	return startContainerResult{operation: operation, container: starting}, nil
}

func failPreparedContainer(
	ctx context.Context,
	store metadata.Store,
	operation metadata.Operation,
	container metadata.Container,
	code chamberErrors.Code,
	cause error,
) (startContainerResult, error) {
	recordErr := failContainerFromCurrent(ctx, store, operation.ID, container.ID, code, cause)
	if updated, err := store.GetContainer(ctx, container.ID); err == nil {
		container = updated
	}
	if updated, err := store.GetOperation(ctx, operation.ID); err == nil {
		operation = updated
	}
	return startContainerResult{operation: operation, container: container}, operationError(operation.ID, code, errors.Join(cause, recordErr))
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
