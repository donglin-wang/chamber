package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

type listOperationsResponse struct {
	Operations      []metadata.Operation `json:"operations"`
	PendingCleanups int                  `json:"pending_cleanups"`
}

func registerOperationRoutes(mux *http.ServeMux, store metadata.Store) {
	mux.HandleFunc("GET /v1/operations", func(w http.ResponseWriter, r *http.Request) {
		operations, err := store.ListOperations(r.Context())
		if err != nil {
			writeDaemonError(w, operationError("", chamberErrors.ErrMetadataFailed, err))
			return
		}
		cleanups, err := store.ListCleanups(r.Context())
		if err != nil {
			writeDaemonError(w, operationError("", chamberErrors.ErrMetadataFailed, err))
			return
		}
		writeJSON(w, http.StatusOK, listOperationsResponse{Operations: operations, PendingCleanups: len(cleanups)})
	})

	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		operationID := strings.TrimSpace(r.PathValue("id"))
		if operationID == "" {
			writeError(w, http.StatusBadRequest, string(chamberErrors.ErrInvalidRequest), "operation id is required")
			return
		}
		operation, err := store.GetOperation(r.Context(), operationID)
		if errors.Is(err, metadata.ErrNotFound) {
			writeError(w, http.StatusNotFound, string(chamberErrors.ErrInvalidRequest), "operation not found")
			return
		}
		if err != nil {
			writeDaemonError(w, operationError(operationID, chamberErrors.ErrMetadataFailed, err))
			return
		}
		writeJSON(w, http.StatusOK, operation)
	})
}

func reconcileRunningOperations(ctx context.Context, store metadata.Store) error {
	operations, err := store.ListOperations(ctx)
	if err != nil {
		return err
	}
	containers, err := store.ListContainers(ctx)
	if err != nil {
		return err
	}
	cleanups, err := store.ListCleanups(ctx)
	if err != nil {
		return err
	}
	containerOperations := make(map[string]metadata.Container, len(containers))
	for _, container := range containers {
		if container.OperationID != "" {
			containerOperations[container.OperationID] = container
		}
	}
	cleanupOperations := make(map[string]struct{}, len(cleanups))
	for _, cleanup := range cleanups {
		cleanupOperations[cleanup.OperationID] = struct{}{}
	}

	var reconcileErr error
	for _, operation := range operations {
		if operation.State != metadata.OperationRunning {
			continue
		}
		switch operation.Kind {
		case metadata.PullOperation:
			// An image record may predate this pull. Without operation-specific
			// completion evidence, recovery cannot claim the pull succeeded.
			_, err = store.FailOperation(ctx, operation.ID, chamberErrors.ErrCanceled)
			reconcileErr = errors.Join(reconcileErr, ignoreTerminalConflict(err))
		case metadata.StopOperation:
			// A terminal container does not prove that this stop delivered its
			// signal. Only operation-specific evidence could justify success.
			_, err = store.FailOperation(ctx, operation.ID, chamberErrors.ErrCanceled)
			reconcileErr = errors.Join(reconcileErr, ignoreTerminalConflict(err))
		case metadata.CreateOperation, metadata.RunOperation, metadata.StartOperation:
			container, linked := containerOperations[operation.ID]
			if !linked {
				_, err = store.FailOperation(ctx, operation.ID, chamberErrors.ErrCanceled)
				reconcileErr = errors.Join(reconcileErr, ignoreTerminalConflict(err))
				continue
			}
			switch container.State {
			case metadata.ContainerCreating, metadata.ContainerStarting, metadata.ContainerRunning:
				// The supervisor and cleanup reconcilers own active container work.
			case metadata.ContainerCreated:
				if operation.Kind == metadata.CreateOperation {
					_, err = store.SucceedOperation(ctx, operation.ID)
				} else {
					_, err = store.FailOperation(ctx, operation.ID, chamberErrors.ErrCanceled)
				}
				reconcileErr = errors.Join(reconcileErr, ignoreTerminalConflict(err))
			case metadata.ContainerExited:
				if container.ExitCode != nil && *container.ExitCode == 0 {
					_, err = store.SucceedOperation(ctx, operation.ID)
				} else {
					code := container.ErrorCode
					if code == "" {
						code = chamberErrors.ErrContainerExitNonzero
					}
					_, err = store.FailOperation(ctx, operation.ID, code)
				}
				reconcileErr = errors.Join(reconcileErr, ignoreTerminalConflict(err))
			case metadata.ContainerFailed:
				code := container.ErrorCode
				if code == "" {
					code = chamberErrors.ErrRuntimeWaitFailed
				}
				_, err = store.FailOperation(ctx, operation.ID, code)
				reconcileErr = errors.Join(reconcileErr, ignoreTerminalConflict(err))
			}
		case metadata.CancelOperation, metadata.RemoveOperation, metadata.CleanupOperation:
			if _, linked := cleanupOperations[operation.ID]; !linked {
				_, err = store.FailOperation(ctx, operation.ID, chamberErrors.ErrCanceled)
				reconcileErr = errors.Join(reconcileErr, ignoreTerminalConflict(err))
			}
		default:
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("unknown running operation kind %q", operation.Kind))
		}
	}
	return reconcileErr
}
