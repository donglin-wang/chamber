package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/google/uuid"
)

const cleanupLeaseDuration = 5 * time.Minute

func admitContainerCleanup(
	ctx context.Context,
	store metadata.Store,
	containerID string,
	kind metadata.OperationKind,
	acquireLease bool,
) (metadata.Cleanup, metadata.Operation, error) {
	container, err := store.GetContainer(ctx, containerID)
	if errors.Is(err, metadata.ErrNotFound) {
		return metadata.Cleanup{}, metadata.Operation{}, operationError("", chamberErrors.ErrContainerNotFound, err)
	}
	if err != nil {
		return metadata.Cleanup{}, metadata.Operation{}, operationError("", chamberErrors.ErrMetadataFailed, err)
	}
	if kind != metadata.DecommissionOperation && kind != metadata.DeleteOperation && kind != metadata.CleanupOperation {
		return metadata.Cleanup{}, metadata.Operation{}, fmt.Errorf("%w: unsupported cleanup operation kind %q", chamberErrors.ErrInvalidRequest, kind)
	}

	operationUUID, err := uuid.NewV7()
	if err != nil {
		return metadata.Cleanup{}, metadata.Operation{}, fmt.Errorf("generate cleanup operation id: %w", err)
	}
	now := time.Now().UTC()
	cleanup := metadata.Cleanup{
		ContainerID:         container.ID,
		OperationID:         operationUUID.String(),
		Kind:                kind,
		SupervisorPID:       container.SupervisorPID,
		SupervisorStartTime: container.SupervisorStartTime,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if acquireLease {
		leaseUUID, err := uuid.NewV7()
		if err != nil {
			return metadata.Cleanup{}, metadata.Operation{}, fmt.Errorf("generate cleanup lease id: %w", err)
		}
		cleanup.LeaseID = leaseUUID.String()
		cleanup.LeaseExpiresAt = now.Add(cleanupLeaseDuration)
	}
	operation := cleanupOperation(cleanup)

	// Persist cleanup intent before its operation. Recovery can reconstruct a
	// missing operation from the cleanup record, while the inverse ordering could
	// leave an unreferenced running operation after a crash.
	if err := store.CreateCleanup(ctx, cleanup); err != nil {
		code := chamberErrors.ErrMetadataFailed
		if errors.Is(err, metadata.ErrAlreadyExists) {
			code = chamberErrors.ErrStateConflict
		}
		return metadata.Cleanup{}, metadata.Operation{}, operationError("", code, err)
	}
	if err := store.CreateOperation(ctx, operation); err != nil {
		return cleanup, operation, operationError(cleanup.OperationID, chamberErrors.ErrMetadataFailed, err)
	}
	return cleanup, operation, nil
}

func cleanupOperation(cleanup metadata.Cleanup) metadata.Operation {
	return metadata.Operation{
		ID:         cleanup.OperationID,
		Kind:       cleanup.Kind,
		State:      metadata.OperationRunning,
		ResourceID: cleanup.ContainerID,
		StartedAt:  cleanup.CreatedAt,
		UpdatedAt:  cleanup.UpdatedAt,
	}
}

func executeContainerCleanup(
	ctx context.Context,
	store metadata.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	openContainer openContainerFunc,
	terminateSupervisor terminateSupervisorFunc,
	cleanup metadata.Cleanup,
) (metadata.Container, metadata.Operation, error) {
	operation, err := ensureCleanupOperation(ctx, store, cleanup)
	if err != nil {
		return metadata.Container{}, metadata.Operation{}, err
	}
	if operation.State != metadata.OperationRunning {
		if err := store.DeleteCleanup(ctx, cleanup.ContainerID, cleanup.LeaseID); err != nil && !errors.Is(err, metadata.ErrNotFound) {
			return metadata.Container{}, operation, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
		}
		return metadata.Container{}, operation, nil
	}

	container, err := store.GetContainer(ctx, cleanup.ContainerID)
	if errors.Is(err, metadata.ErrNotFound) && cleanup.Kind == metadata.DeleteOperation {
		completed, completeErr := completeContainerCleanup(ctx, store, cleanup, operation)
		return metadata.Container{ID: cleanup.ContainerID, OperationID: operation.ID}, completed, completeErr
	}
	if errors.Is(err, metadata.ErrNotFound) {
		return metadata.Container{}, operation, operationError(operation.ID, chamberErrors.ErrContainerNotFound, err)
	}
	if err != nil {
		return metadata.Container{}, operation, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
	}

	cleanupTarget := container
	if cleanup.SupervisorPID != 0 {
		cleanupTarget.SupervisorPID = cleanup.SupervisorPID
		cleanupTarget.SupervisorStartTime = cleanup.SupervisorStartTime
	}
	if container.State != metadata.ContainerExited &&
		container.State != metadata.ContainerFailed &&
		container.State != metadata.ContainerDecommissioned &&
		!(cleanup.Kind == metadata.DecommissionOperation && container.State == metadata.ContainerCreated) {
		originalOperationID := container.OperationID
		now := time.Now().UTC()
		updated, _, transitionErr := store.TransitionContainerAndOperation(
			ctx,
			container.ID,
			container.State,
			metadata.ContainerUpdate{
				State: metadata.ContainerFailed, At: now, ErrorCode: chamberErrors.ErrCanceled, ClearSupervisor: true,
			},
			originalOperationID,
			metadata.OperationRunning,
			metadata.OperationUpdate{State: metadata.OperationAborted, At: now, ErrorCode: chamberErrors.ErrCanceled},
		)
		if transitionErr != nil {
			if errors.Is(transitionErr, chamberErrors.ErrStateConflict) {
				container, transitionErr = store.GetContainer(ctx, container.ID)
				if transitionErr == nil && container.State != metadata.ContainerExited && container.State != metadata.ContainerFailed {
					container, transitionErr = store.TransitionContainer(ctx, container.ID, container.State, metadata.ContainerUpdate{
						State: metadata.ContainerFailed, At: now, ErrorCode: chamberErrors.ErrCanceled, ClearSupervisor: true,
					})
				}
			}
		} else {
			container = updated
		}
		if transitionErr != nil {
			return container, operation, operationError(operation.ID, chamberErrors.ErrMetadataFailed, transitionErr)
		}
	}

	cleanupContainer := cleanupTarget
	cleanupContainer.OperationID = operation.ID
	removeLogsAndEvidence := cleanup.Kind == metadata.DeleteOperation
	if err := deleteContainerArtifacts(ctx, runtimeConfig, provisioner, openContainer, terminateSupervisor, cleanupContainer, removeLogsAndEvidence); err != nil {
		code := chamberErrors.CodeFromError(err, chamberErrors.ErrRuntimeControlFailed)
		return container, operation, operationError(operation.ID, code, err)
	}

	if cleanup.Kind == metadata.DecommissionOperation && container.State != metadata.ContainerDecommissioned {
		now := time.Now().UTC()
		decommissioned, completed, err := store.TransitionContainerAndOperation(
			ctx,
			container.ID,
			container.State,
			metadata.ContainerUpdate{
				State:           metadata.ContainerDecommissioned,
				At:              now,
				ExitCode:        container.ExitCode,
				ErrorCode:       container.ErrorCode,
				ClearSupervisor: true,
			},
			operation.ID,
			metadata.OperationRunning,
			metadata.OperationUpdate{State: metadata.OperationSucceeded, At: now},
		)
		if err != nil {
			return container, operation, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
		}
		container = decommissioned
		operation = completed
	}

	if cleanup.Kind == metadata.DeleteOperation {
		removed, err := store.DeleteContainer(ctx, container.ID)
		if err != nil && !errors.Is(err, metadata.ErrNotFound) {
			return container, operation, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
		}
		if err == nil {
			container = removed
		}
	}

	completed, err := completeContainerCleanup(ctx, store, cleanup, operation)
	return container, completed, err
}

func ensureCleanupOperation(ctx context.Context, store metadata.Store, cleanup metadata.Cleanup) (metadata.Operation, error) {
	operation, err := store.GetOperation(ctx, cleanup.OperationID)
	if err == nil {
		return operation, nil
	}
	if !errors.Is(err, metadata.ErrNotFound) {
		return metadata.Operation{}, operationError(cleanup.OperationID, chamberErrors.ErrMetadataFailed, err)
	}
	operation = cleanupOperation(cleanup)
	if err := store.CreateOperation(ctx, operation); err != nil && !errors.Is(err, metadata.ErrAlreadyExists) {
		return metadata.Operation{}, operationError(cleanup.OperationID, chamberErrors.ErrMetadataFailed, err)
	}
	return store.GetOperation(ctx, cleanup.OperationID)
}

func completeContainerCleanup(
	ctx context.Context,
	store metadata.Store,
	cleanup metadata.Cleanup,
	operation metadata.Operation,
) (metadata.Operation, error) {
	completed := operation
	if operation.State == metadata.OperationRunning {
		var err error
		completed, err = store.SucceedOperation(ctx, operation.ID)
		if err != nil && !errors.Is(err, chamberErrors.ErrStateConflict) {
			return operation, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
		}
		if errors.Is(err, chamberErrors.ErrStateConflict) {
			completed, err = store.GetOperation(ctx, operation.ID)
			if err != nil {
				return operation, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
			}
		}
	}
	if err := store.DeleteCleanup(ctx, cleanup.ContainerID, cleanup.LeaseID); err != nil && !errors.Is(err, metadata.ErrNotFound) {
		return completed, operationError(operation.ID, chamberErrors.ErrMetadataFailed, err)
	}
	return completed, nil
}

func reconcileContainerCleanups(
	ctx context.Context,
	store metadata.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	openContainer openContainerFunc,
	terminateSupervisor terminateSupervisorFunc,
	reclaim bool,
) error {
	cleanups, err := store.ListCleanups(ctx)
	if err != nil {
		return err
	}
	var reconcileErr error
	for _, pending := range cleanups {
		pending := pending
		daemonOperationLocks.with("container:"+pending.ContainerID, func() {
			leaseUUID, err := uuid.NewV7()
			if err != nil {
				reconcileErr = errors.Join(reconcileErr, err)
				return
			}
			now := time.Now().UTC()
			cleanup, err := store.ClaimCleanup(ctx, pending.ContainerID, leaseUUID.String(), now, now.Add(cleanupLeaseDuration), reclaim)
			if errors.Is(err, metadata.ErrLeaseHeld) || errors.Is(err, metadata.ErrNotFound) {
				return
			}
			if err != nil {
				reconcileErr = errors.Join(reconcileErr, err)
				return
			}
			_, _, err = executeContainerCleanup(ctx, store, runtimeConfig, provisioner, openContainer, terminateSupervisor, cleanup)
			if err != nil {
				reconcileErr = errors.Join(reconcileErr, err)
			}
		})
	}
	return reconcileErr
}

func watchContainerCleanups(
	ctx context.Context,
	store metadata.Store,
	runtimeConfig chamberRuntime.Config,
	provisioner chamberBundle.Provisioner,
	openContainer openContainerFunc,
	terminateSupervisor terminateSupervisorFunc,
) {
	ticker := time.NewTicker(supervisorReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reconcileContainerCleanups(ctx, store, runtimeConfig, provisioner, openContainer, terminateSupervisor, false); err != nil {
				fmt.Fprintf(os.Stderr, "reconcile container cleanups: error=%v\n", err)
			}
		}
	}
}
