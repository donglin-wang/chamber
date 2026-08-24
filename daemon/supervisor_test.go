package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	"github.com/donglin-wang/chamber/daemon/metadata/memory"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestReconcileSupervisorAppliesCompletedPhase(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerRunning, time.Now().UTC())
	exitCode := 0
	exitedAt := time.Now().UTC()
	state := supervisorStateForContainer(t, container, supervisorCompleted)
	state.Result = &chamberRuntime.ContainerResult{
		ContainerID: container.ID,
		Status:      chamberRuntime.ContainerResultStatusExited,
		ExitCode:    &exitCode,
		ExitedAt:    exitedAt,
	}
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v", err)
	}

	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerExited {
		t.Fatalf("container state = %q, want %q", updated.State, metadata.ContainerExited)
	}
	if updated.ExitCode == nil || *updated.ExitCode != 0 {
		t.Fatalf("container exit code = %v, want 0", updated.ExitCode)
	}
	operation, err := store.GetOperation(context.Background(), container.OperationID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if operation.State != metadata.OperationSucceeded {
		t.Fatalf("operation state = %q, want %q", operation.State, metadata.OperationSucceeded)
	}
}

func TestReconcileSupervisorRejectsWrongContainerResult(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerRunning, time.Now().UTC())
	exitCode := 0
	state := supervisorStateForContainer(t, container, supervisorCompleted)
	state.Result = &chamberRuntime.ContainerResult{
		ContainerID: "other-container",
		Status:      chamberRuntime.ContainerResultStatusExited,
		ExitCode:    &exitCode,
		ExitedAt:    time.Now().UTC(),
	}
	if err := writeSupervisorJSON(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorJSON() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container); err == nil {
		t.Fatal("reconcileSupervisorContainer() error = nil, want validation error")
	}

	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerFailed {
		t.Fatalf("container state = %q, want %q", updated.State, metadata.ContainerFailed)
	}
	if updated.ErrorCode != chamberErrors.ErrRuntimeWaitFailed {
		t.Fatalf("container error code = %q, want %q", updated.ErrorCode, chamberErrors.ErrRuntimeWaitFailed)
	}
}

func TestReconcileSupervisorRecordsStartedPhaseFromCreating(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreating, time.Now().UTC())
	state := supervisorStateForContainer(t, container, supervisorStarted)
	startedAt := time.Now().UTC()
	state.RuntimeName = container.Runtime
	state.StartedAt = &startedAt
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v", err)
	}

	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerRunning {
		t.Fatalf("container state = %q, want %q", updated.State, metadata.ContainerRunning)
	}
}

func TestReconcileSupervisorFailsStalePreparedContainer(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreating, time.Now().UTC().Add(-supervisorCreatingTimeout-time.Second))
	if err := writeSupervisorFile(container.SupervisorPath, supervisorStateForContainer(t, container, supervisorPrepared)); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container); err == nil {
		t.Fatal("reconcileSupervisorContainer() error = nil, want stale prepared error")
	}

	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerFailed {
		t.Fatalf("container state = %q, want %q", updated.State, metadata.ContainerFailed)
	}
	if updated.ErrorCode != chamberErrors.ErrRuntimeStartFailed {
		t.Fatalf("container error code = %q, want %q", updated.ErrorCode, chamberErrors.ErrRuntimeStartFailed)
	}
}

func createSupervisorContainer(t *testing.T, store metadata.Store, state metadata.ContainerState, updatedAt time.Time) metadata.Container {
	t.Helper()

	operationID := "operation-" + string(state)
	containerID := "container-" + string(state)
	operation := metadata.Operation{
		ID:         operationID,
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: containerID,
		StartedAt:  updatedAt,
		UpdatedAt:  updatedAt,
	}
	if err := store.CreateOperation(context.Background(), operation); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}
	root := t.TempDir()
	container := metadata.Container{
		ID:             containerID,
		OperationID:    operationID,
		ImageDigest:    "sha256:image",
		ImageRef:       "docker.io/library/alpine:latest",
		BundlePath:     filepath.Join(root, "bundle"),
		StdoutPath:     filepath.Join(root, "stdout.log"),
		StderrPath:     filepath.Join(root, "stderr.log"),
		Runtime:        "fake",
		RuntimeRoot:    filepath.Join(root, "runtime"),
		SupervisorPath: filepath.Join(root, "supervisor.json"),
		State:          state,
		CreatedAt:      updatedAt,
		UpdatedAt:      updatedAt,
	}
	if err := store.CreateContainer(context.Background(), container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}
	return container
}

func supervisorStateForContainer(t *testing.T, container metadata.Container, phase supervisorPhase) supervisorFile {
	t.Helper()

	now := time.Now().UTC()
	return supervisorFile{
		Version:     supervisorFileVersion,
		Phase:       phase,
		OperationID: container.OperationID,
		ContainerID: container.ID,
		Runtime: chamberRuntime.Config{
			Name:        container.Runtime,
			RuntimeRoot: container.RuntimeRoot,
		},
		Bundle: chamberBundle.ProvisionedBundle{
			ContainerID: container.ID,
			BundlePath:  container.BundlePath,
		},
		StdoutPath: container.StdoutPath,
		StderrPath: container.StderrPath,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}
