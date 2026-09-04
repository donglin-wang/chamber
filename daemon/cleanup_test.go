package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	"github.com/donglin-wang/chamber/daemon/metadata/memory"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestReconcileCleanupReconstructsOperationAndFinishesForcedRemove(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)
	now := time.Now().UTC()
	cleanup := metadata.Cleanup{
		ContainerID: container.ID,
		OperationID: "operation-recovered-remove",
		Kind:        metadata.RemoveOperation,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.CreateCleanup(context.Background(), cleanup); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}

	var removed chamberBundle.ProvisionedBundle
	handle := &fakeContainerHandle{id: container.ID}
	err := reconcileContainerCleanups(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{remove: &removed},
		func(context.Context, chamberRuntime.Config, string) (chamberRuntime.ContainerHandle, error) {
			return handle, nil
		},
		nil,
		false,
	)
	if err != nil {
		t.Fatalf("reconcileContainerCleanups() error = %v", err)
	}
	if _, err := store.GetContainer(context.Background(), container.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetContainer() error = %v, want not found", err)
	}
	operation, err := store.GetOperation(context.Background(), cleanup.OperationID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if operation.State != metadata.OperationSucceeded || operation.Kind != metadata.RemoveOperation {
		t.Fatalf("cleanup operation = %#v, want succeeded remove", operation)
	}
	cleanups, err := store.ListCleanups(context.Background())
	if err != nil || len(cleanups) != 0 {
		t.Fatalf("ListCleanups() = %#v, %v; want empty", cleanups, err)
	}
	if !handle.deleted || !handle.deleteForce {
		t.Fatalf("runtime handle = %#v, want forced delete", handle)
	}
	if removed.ContainerID != container.ID || removed.BundlePath != container.BundlePath {
		t.Fatalf("removed bundle = %#v, want container bundle", removed)
	}
}

func TestRemoveCleanupDeletesContainerWhenSourceOperationIsAlreadyTerminal(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)
	if _, err := store.SucceedOperation(context.Background(), container.OperationID); err != nil {
		t.Fatalf("SucceedOperation(source) error = %v", err)
	}
	now := time.Now().UTC()
	cleanup := metadata.Cleanup{
		ContainerID: container.ID,
		OperationID: "operation-terminal-source-remove",
		Kind:        metadata.RemoveOperation,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.CreateCleanup(context.Background(), cleanup); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}
	if err := store.CreateOperation(context.Background(), cleanupOperation(cleanup)); err != nil {
		t.Fatalf("CreateOperation(cleanup) error = %v", err)
	}

	if err := reconcileContainerCleanups(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{},
		fakeOpenContainer(nil),
		nil,
		false,
	); err != nil {
		t.Fatalf("reconcileContainerCleanups() error = %v", err)
	}
	if _, err := store.GetContainer(context.Background(), container.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetContainer() error = %v, want not found", err)
	}
	operation, err := store.GetOperation(context.Background(), cleanup.OperationID)
	if err != nil {
		t.Fatalf("GetOperation(cleanup) error = %v", err)
	}
	if operation.State != metadata.OperationSucceeded {
		t.Fatalf("cleanup operation state = %q, want %q", operation.State, metadata.OperationSucceeded)
	}
	cleanups, err := store.ListCleanups(context.Background())
	if err != nil || len(cleanups) != 0 {
		t.Fatalf("ListCleanups() = %#v, %v; want empty", cleanups, err)
	}
}

func TestCancelCleanupFailsContainerWhenSourceOperationIsAlreadyTerminal(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)
	if _, err := store.SucceedOperation(context.Background(), container.OperationID); err != nil {
		t.Fatalf("SucceedOperation(source) error = %v", err)
	}
	now := time.Now().UTC()
	cleanup := metadata.Cleanup{
		ContainerID: container.ID,
		OperationID: "operation-terminal-source-cancel",
		Kind:        metadata.CancelOperation,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.CreateCleanup(context.Background(), cleanup); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}
	if err := store.CreateOperation(context.Background(), cleanupOperation(cleanup)); err != nil {
		t.Fatalf("CreateOperation(cleanup) error = %v", err)
	}

	if err := reconcileContainerCleanups(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{},
		fakeOpenContainer(nil),
		nil,
		false,
	); err != nil {
		t.Fatalf("reconcileContainerCleanups() error = %v", err)
	}
	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerFailed || updated.ErrorCode != chamberErrors.ErrCanceled {
		t.Fatalf("container = %#v, want failed with canceled error", updated)
	}
	operation, err := store.GetOperation(context.Background(), cleanup.OperationID)
	if err != nil {
		t.Fatalf("GetOperation(cleanup) error = %v", err)
	}
	if operation.State != metadata.OperationSucceeded {
		t.Fatalf("cleanup operation state = %q, want %q", operation.State, metadata.OperationSucceeded)
	}
}

func TestPeriodicCleanupReconciliationRespectsActiveLease(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerExited)
	now := time.Now().UTC()
	cleanup := metadata.Cleanup{
		ContainerID:    container.ID,
		OperationID:    "operation-leased-remove",
		Kind:           metadata.RemoveOperation,
		LeaseID:        "request-lease",
		LeaseExpiresAt: now.Add(time.Minute),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := store.CreateCleanup(context.Background(), cleanup); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}
	if err := store.CreateOperation(context.Background(), cleanupOperation(cleanup)); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}

	provisioner := fakeProvisioner{}
	if err := reconcileContainerCleanups(context.Background(), store, chamberRuntime.Config{Name: "fake"}, provisioner, fakeOpenContainer(nil), nil, false); err != nil {
		t.Fatalf("periodic reconcile error = %v", err)
	}
	if _, err := store.GetContainer(context.Background(), container.ID); err != nil {
		t.Fatalf("active lease container disappeared: %v", err)
	}
	if err := reconcileContainerCleanups(context.Background(), store, chamberRuntime.Config{Name: "fake"}, provisioner, fakeOpenContainer(nil), nil, true); err != nil {
		t.Fatalf("startup reclaim error = %v", err)
	}
	if _, err := store.GetContainer(context.Background(), container.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetContainer(after reclaim) error = %v, want not found", err)
	}
}

func TestRecoveryCleanupRetainsSupervisorEvidence(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerFailed)
	if err := os.MkdirAll(filepath.Dir(container.SupervisorPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(supervisor) error = %v", err)
	}
	if err := os.WriteFile(container.SupervisorPath, []byte("evidence"), 0o600); err != nil {
		t.Fatalf("WriteFile(supervisor) error = %v", err)
	}
	now := time.Now().UTC()
	cleanup := metadata.Cleanup{
		ContainerID: container.ID,
		OperationID: "operation-recovery-cleanup",
		Kind:        metadata.CleanupOperation,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.CreateCleanup(context.Background(), cleanup); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}
	if err := store.CreateOperation(context.Background(), cleanupOperation(cleanup)); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}

	if err := reconcileContainerCleanups(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{},
		fakeOpenContainer(nil),
		nil,
		false,
	); err != nil {
		t.Fatalf("reconcileContainerCleanups() error = %v", err)
	}
	if _, err := os.Stat(container.SupervisorPath); err != nil {
		t.Fatalf("supervisor evidence was not retained: %v", err)
	}
	if _, err := store.GetContainer(context.Background(), container.ID); err != nil {
		t.Fatalf("failed container record was not retained: %v", err)
	}
}

func TestRecoveryCleanupUsesDurableSupervisorIdentityAfterContainerTerminalization(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)
	now := time.Now().UTC()
	cleanup := metadata.Cleanup{
		ContainerID:         container.ID,
		OperationID:         "operation-recovery-supervisor-identity",
		Kind:                metadata.CleanupOperation,
		SupervisorPID:       container.SupervisorPID,
		SupervisorStartTime: container.SupervisorStartTime,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := store.CreateCleanup(context.Background(), cleanup); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}
	if err := store.CreateOperation(context.Background(), cleanupOperation(cleanup)); err != nil {
		t.Fatalf("CreateOperation(cleanup) error = %v", err)
	}
	if _, _, err := store.TransitionContainerAndOperation(
		context.Background(),
		container.ID,
		metadata.ContainerRunning,
		metadata.ContainerUpdate{State: metadata.ContainerFailed, At: now, ErrorCode: chamberErrors.ErrCanceled, ClearSupervisor: true},
		container.OperationID,
		metadata.OperationRunning,
		metadata.OperationUpdate{State: metadata.OperationAborted, At: now, ErrorCode: chamberErrors.ErrCanceled},
	); err != nil {
		t.Fatalf("terminalize before simulated crash: %v", err)
	}

	var terminatedPID int
	var terminatedStartTime uint64
	if err := reconcileContainerCleanups(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{},
		fakeOpenContainer(nil),
		func(pid int, startTime uint64) error {
			terminatedPID = pid
			terminatedStartTime = startTime
			return nil
		},
		true,
	); err != nil {
		t.Fatalf("reconcileContainerCleanups() error = %v", err)
	}
	if terminatedPID != cleanup.SupervisorPID || terminatedStartTime != cleanup.SupervisorStartTime {
		t.Fatalf("terminated identity = (%d, %d), want (%d, %d)", terminatedPID, terminatedStartTime, cleanup.SupervisorPID, cleanup.SupervisorStartTime)
	}
}
