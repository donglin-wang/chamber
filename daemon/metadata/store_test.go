package metadata_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	metadataetcd "github.com/donglin-wang/chamber/daemon/metadata/etcd"
	"github.com/donglin-wang/chamber/daemon/metadata/memory"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

type traceContextKey struct{}

func TestContainerValidTransition(t *testing.T) {
	tests := map[metadata.StateTransition[metadata.ContainerState]]bool{
		{From: metadata.ContainerCreating, To: metadata.ContainerStarting}:      true,
		{From: metadata.ContainerCreating, To: metadata.ContainerCreated}:       true,
		{From: metadata.ContainerCreating, To: metadata.ContainerFailed}:        true,
		{From: metadata.ContainerCreated, To: metadata.ContainerStarting}:       true,
		{From: metadata.ContainerCreated, To: metadata.ContainerFailed}:         true,
		{From: metadata.ContainerCreated, To: metadata.ContainerDecommissioned}: true,
		{From: metadata.ContainerStarting, To: metadata.ContainerRunning}:       true,
		{From: metadata.ContainerStarting, To: metadata.ContainerFailed}:        true,
		{From: metadata.ContainerStarting, To: metadata.ContainerExited}:        true,
		{From: metadata.ContainerRunning, To: metadata.ContainerExited}:         true,
		{From: metadata.ContainerRunning, To: metadata.ContainerFailed}:         true,
		{From: metadata.ContainerExited, To: metadata.ContainerDecommissioned}:  true,
		{From: metadata.ContainerFailed, To: metadata.ContainerDecommissioned}:  true,

		{From: metadata.ContainerCreating, To: metadata.ContainerRunning}:        false,
		{From: metadata.ContainerCreated, To: metadata.ContainerRunning}:         false,
		{From: metadata.ContainerRunning, To: metadata.ContainerStarting}:        false,
		{From: metadata.ContainerExited, To: metadata.ContainerRunning}:          false,
		{From: metadata.ContainerFailed, To: metadata.ContainerRunning}:          false,
		{From: metadata.ContainerDecommissioned, To: metadata.ContainerStarting}: false,
		{From: metadata.ContainerDecommissioned, To: metadata.ContainerRunning}:  false,
		{From: metadata.ContainerRunning, To: metadata.ContainerRunning}:         false,
		{From: metadata.ContainerState("weird"), To: metadata.ContainerRunning}:  false,
	}

	for transition, expected := range tests {
		result := metadata.IsContainerTransitionValid(transition.From, transition.To)
		if result != expected {
			t.Fatalf("IsContainerTransitionValid(%q, %q) returned %v, expected %v", transition.From, transition.To, result, expected)
		}
	}
}

func TestOperationValidTransition(t *testing.T) {
	tests := map[metadata.StateTransition[metadata.OperationState]]bool{
		{From: metadata.OperationRunning, To: metadata.OperationSucceeded}: true,
		{From: metadata.OperationRunning, To: metadata.OperationFailed}:    true,
		{From: metadata.OperationRunning, To: metadata.OperationAborted}:   true,

		{From: metadata.OperationSucceeded, To: metadata.OperationRunning}:        false,
		{From: metadata.OperationSucceeded, To: metadata.OperationFailed}:         false,
		{From: metadata.OperationSucceeded, To: metadata.OperationAborted}:        false,
		{From: metadata.OperationSucceeded, To: metadata.OperationSucceeded}:      false,
		{From: metadata.OperationFailed, To: metadata.OperationRunning}:           false,
		{From: metadata.OperationFailed, To: metadata.OperationSucceeded}:         false,
		{From: metadata.OperationFailed, To: metadata.OperationAborted}:           false,
		{From: metadata.OperationFailed, To: metadata.OperationFailed}:            false,
		{From: metadata.OperationAborted, To: metadata.OperationRunning}:          false,
		{From: metadata.OperationAborted, To: metadata.OperationSucceeded}:        false,
		{From: metadata.OperationAborted, To: metadata.OperationFailed}:           false,
		{From: metadata.OperationAborted, To: metadata.OperationAborted}:          false,
		{From: metadata.OperationState("weird"), To: metadata.OperationRunning}:   false,
		{From: metadata.OperationRunning, To: metadata.OperationState("weird")}:   false,
		{From: metadata.OperationState("weird"), To: metadata.OperationSucceeded}: false,
	}

	for transition, expected := range tests {
		result := metadata.IsOperationTransitionValid(transition.From, transition.To)
		if result != expected {
			t.Fatalf("IsOperationTransitionValid(%q, %q) returned %v, expected %v", transition.From, transition.To, result, expected)
		}
	}
}

func TestStoreContract(t *testing.T) {
	tests := map[string]func(t *testing.T) metadata.Store{
		"memory": func(t *testing.T) metadata.Store {
			t.Helper()
			return memory.NewMemoryStore()
		},
		"etcd": func(t *testing.T) metadata.Store {
			t.Helper()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			testRoot := t.TempDir()

			dataDir := filepath.Join(testRoot, "data")
			store, err := metadataetcd.Open(ctx, metadata.Config{
				Root: dataDir,
			}, newTestWorkspace(t, dataDir, filepath.Join(testRoot, "tmp")))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			return store
		},
	}

	for name, newStore := range tests {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			t.Cleanup(func() { _ = store.Close() })

			assertImageRoundTrip(t, store)
			assertOperationLifecycle(t, store)
			assertContainerLifecycle(t, store)
			assertAtomicContainerAdmission(t, store)
			assertAtomicStartAdmission(t, store)
			assertAtomicContainerOperationTransition(t, store)
			assertCleanupLeaseLifecycle(t, store)
			assertTerminalHelpers(t, store)
			assertConcurrentOperationCreate(t, store)
			assertConcurrentOperationTransition(t, store)
			assertConcurrentTerminalOperationTransition(t, store)
			assertConcurrentContainerTransition(t, store)
			assertTraceFieldsAreExplicit(t, store)
		})
	}
}

func assertAtomicStartAdmission(t *testing.T, store metadata.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 10, 11, 30, 0, time.UTC)
	container := metadata.Container{
		ID: "container-atomic-start", OperationID: "op-atomic-create", State: metadata.ContainerCreated,
		ImageDigest: "sha256:start", ImageRef: "docker.io/library/alpine:latest", Runtime: "runc", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateContainer(ctx, container); err != nil {
		t.Fatalf("CreateContainer(start) error = %v", err)
	}
	operation := metadata.Operation{
		ID: "op-atomic-start", Kind: metadata.StartOperation, State: metadata.OperationRunning,
		ResourceID: container.ID, StartedAt: now, UpdatedAt: now,
	}
	started, err := store.CreateOperationAndTransitionContainer(ctx, operation, container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
		OperationID: operation.ID, State: metadata.ContainerStarting, At: now,
	})
	if err != nil {
		t.Fatalf("CreateOperationAndTransitionContainer() error = %v", err)
	}
	if started.State != metadata.ContainerStarting || started.OperationID != operation.ID {
		t.Fatalf("started container = %#v", started)
	}
	conflict := operation
	conflict.ID = "op-atomic-start-conflict"
	if _, err := store.CreateOperationAndTransitionContainer(ctx, conflict, container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
		OperationID: conflict.ID, State: metadata.ContainerStarting, At: now,
	}); !errors.Is(err, chamberErrors.ErrStateConflict) {
		t.Fatalf("CreateOperationAndTransitionContainer(conflict) error = %v, want state conflict", err)
	}
	if _, err := store.GetOperation(ctx, conflict.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetOperation(partial start) error = %v, want not found", err)
	}
}

func assertAtomicContainerOperationTransition(t *testing.T, store metadata.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 10, 11, 45, 0, time.UTC)
	operation := metadata.Operation{
		ID: "op-atomic-terminal", Kind: metadata.RunOperation, State: metadata.OperationRunning,
		ResourceID: "container-atomic-terminal", StartedAt: now, UpdatedAt: now,
	}
	container := metadata.Container{
		ID: operation.ResourceID, OperationID: operation.ID, State: metadata.ContainerRunning,
		ImageDigest: "sha256:terminal", ImageRef: "docker.io/library/alpine:latest", Runtime: "runc",
		SupervisorPID: 123, SupervisorStartTime: 456, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateContainerAndOperation(ctx, container, operation); err != nil {
		t.Fatalf("CreateContainerAndOperation(terminal) error = %v", err)
	}
	exitCode := 0
	finished := now.Add(time.Second)
	updatedContainer, updatedOperation, err := store.TransitionContainerAndOperation(
		ctx, container.ID, metadata.ContainerRunning,
		metadata.ContainerUpdate{State: metadata.ContainerExited, At: finished, ExitCode: &exitCode, ClearSupervisor: true},
		operation.ID, metadata.OperationRunning,
		metadata.OperationUpdate{State: metadata.OperationSucceeded, At: finished},
	)
	if err != nil {
		t.Fatalf("TransitionContainerAndOperation() error = %v", err)
	}
	if updatedContainer.State != metadata.ContainerExited || updatedContainer.SupervisorPID != 0 || updatedContainer.SupervisorStartTime != 0 {
		t.Fatalf("terminal container = %#v", updatedContainer)
	}
	if updatedOperation.State != metadata.OperationSucceeded {
		t.Fatalf("terminal operation = %#v", updatedOperation)
	}

	operations, err := store.ListOperations(ctx)
	if err != nil {
		t.Fatalf("ListOperations() error = %v", err)
	}
	found := false
	for _, listed := range operations {
		if listed.ID == operation.ID && listed.State == metadata.OperationSucceeded {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListOperations() did not include terminal operation %q", operation.ID)
	}
}

func assertAtomicContainerAdmission(t *testing.T, store metadata.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 10, 11, 0, 0, time.UTC)
	operation := metadata.Operation{
		ID:         "op-atomic-admission",
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-atomic-admission",
		StartedAt:  now,
		UpdatedAt:  now,
	}
	container := metadata.Container{
		ID:          operation.ResourceID,
		OperationID: operation.ID,
		ImageDigest: "sha256:atomic",
		ImageRef:    "docker.io/library/alpine:latest",
		Runtime:     "runc",
		State:       metadata.ContainerCreating,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.CreateContainerAndOperation(ctx, container, operation); err != nil {
		t.Fatalf("CreateContainerAndOperation() error = %v", err)
	}
	if _, err := store.GetContainer(ctx, container.ID); err != nil {
		t.Fatalf("GetContainer(admitted) error = %v", err)
	}
	if _, err := store.GetOperation(ctx, operation.ID); err != nil {
		t.Fatalf("GetOperation(admitted) error = %v", err)
	}

	conflictingOperation := operation
	conflictingOperation.ID = "op-atomic-conflict"
	if err := store.CreateContainerAndOperation(ctx, container, conflictingOperation); !errors.Is(err, metadata.ErrAlreadyExists) {
		t.Fatalf("CreateContainerAndOperation(conflict) error = %v, want already exists", err)
	}
	if _, err := store.GetOperation(ctx, conflictingOperation.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetOperation(partial admission) error = %v, want not found", err)
	}
}

func assertCleanupLeaseLifecycle(t *testing.T, store metadata.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 10, 13, 0, 0, time.UTC)
	cleanup := metadata.Cleanup{
		ContainerID:    "container-cleanup-lease",
		OperationID:    "op-cleanup-lease",
		Kind:           metadata.DeleteOperation,
		LeaseID:        "lease-original",
		LeaseExpiresAt: now.Add(time.Minute),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := store.CreateCleanup(ctx, cleanup); err != nil {
		t.Fatalf("CreateCleanup() error = %v", err)
	}
	cleanups, err := store.ListCleanups(ctx)
	if err != nil {
		t.Fatalf("ListCleanups() error = %v", err)
	}
	if len(cleanups) != 1 || cleanups[0] != cleanup {
		t.Fatalf("ListCleanups() = %#v, want %#v", cleanups, cleanup)
	}
	if _, err := store.ClaimCleanup(ctx, cleanup.ContainerID, "lease-other", now, now.Add(time.Minute), false); !errors.Is(err, metadata.ErrLeaseHeld) {
		t.Fatalf("ClaimCleanup(active lease) error = %v, want lease held", err)
	}
	reclaimed, err := store.ClaimCleanup(ctx, cleanup.ContainerID, "lease-recovery", now, now.Add(time.Minute), true)
	if err != nil {
		t.Fatalf("ClaimCleanup(reclaim) error = %v", err)
	}
	if reclaimed.LeaseID != "lease-recovery" {
		t.Fatalf("reclaimed lease = %q, want lease-recovery", reclaimed.LeaseID)
	}
	if err := store.DeleteCleanup(ctx, cleanup.ContainerID, "lease-other"); !errors.Is(err, metadata.ErrLeaseHeld) {
		t.Fatalf("DeleteCleanup(wrong lease) error = %v, want lease held", err)
	}
	if err := store.DeleteCleanup(ctx, cleanup.ContainerID, reclaimed.LeaseID); err != nil {
		t.Fatalf("DeleteCleanup() error = %v", err)
	}
	cleanups, err = store.ListCleanups(ctx)
	if err != nil || len(cleanups) != 0 {
		t.Fatalf("ListCleanups(after delete) = %#v, %v; want empty", cleanups, err)
	}
}

func newTestWorkspace(t *testing.T, root string, tmpRoot string) *hostfs.Workspace {
	t.Helper()
	workspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:    root,
		TmpRoot: tmpRoot,
		Requirements: hostfs.FeatureSet{
			PrivateDirs:      true,
			FileFsync:        true,
			AtomicFileRename: true,
		},
	})
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	return workspace
}

func assertImageRoundTrip(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	pulledAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	image := metadata.Image{
		Reference:  "docker.io/library/alpine:latest",
		Digest:     "sha256:abc123",
		Platform:   chamberImage.Platform{OS: "linux", Architecture: "arm64"},
		PulledAt:   pulledAt,
		LastUsedAt: pulledAt,
	}

	if err := store.PutImage(ctx, image); err != nil {
		t.Fatalf("PutImage() error = %v", err)
	}

	got, err := store.GetImage(ctx, image.Reference)
	if err != nil {
		t.Fatalf("GetImage() error = %v", err)
	}
	if got != image {
		t.Fatalf("GetImage() = %#v, want %#v", got, image)
	}

	if _, err := store.GetImage(ctx, "missing"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetImage(missing) error = %v, want %v", err, metadata.ErrNotFound)
	}
}

func assertOperationLifecycle(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	startedAt := time.Date(2026, 7, 11, 10, 5, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Second)
	operation := metadata.Operation{
		ID:         "op-1",
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-1",
		TraceID:    "trace-1",
		SpanID:     "span-1",
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}

	if err := store.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}
	if err := store.CreateOperation(ctx, operation); !errors.Is(err, metadata.ErrAlreadyExists) {
		t.Fatalf("CreateOperation(duplicate) error = %v, want %v", err, metadata.ErrAlreadyExists)
	}

	got, err := store.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if got != operation {
		t.Fatalf("GetOperation() = %#v, want %#v", got, operation)
	}

	updated, err := store.TransitionOperation(ctx, operation.ID, metadata.OperationRunning, metadata.OperationUpdate{
		State: metadata.OperationSucceeded,
		At:    finishedAt,
	})
	if err != nil {
		t.Fatalf("TransitionOperation() error = %v", err)
	}
	if updated.State != metadata.OperationSucceeded {
		t.Fatalf("TransitionOperation() state = %q, want %q", updated.State, metadata.OperationSucceeded)
	}
	if updated.FinishedAt == nil || !updated.FinishedAt.Equal(finishedAt) {
		t.Fatalf("TransitionOperation() FinishedAt = %v, want %v", updated.FinishedAt, finishedAt)
	}

	updated.FinishedAt = nil
	reread, err := store.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatalf("GetOperation(after caller mutation) error = %v", err)
	}
	if reread.FinishedAt == nil {
		t.Fatal("GetOperation(after caller mutation) FinishedAt = nil, want stored timestamp")
	}

	_, err = store.TransitionOperation(ctx, operation.ID, metadata.OperationRunning, metadata.OperationUpdate{
		State: metadata.OperationFailed,
		At:    finishedAt.Add(time.Second),
	})
	if !errors.Is(err, chamberErrors.ErrStateConflict) {
		t.Fatalf("TransitionOperation(stale from) error = %v, want %v", err, chamberErrors.ErrStateConflict)
	}

	_, err = store.TransitionOperation(ctx, operation.ID, metadata.OperationSucceeded, metadata.OperationUpdate{
		State: metadata.OperationFailed,
		At:    finishedAt.Add(time.Second),
	})
	if !errors.Is(err, chamberErrors.ErrStateConflict) {
		t.Fatalf("TransitionOperation(invalid transition) error = %v, want %v", err, chamberErrors.ErrStateConflict)
	}
}

func assertTerminalHelpers(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	startedAt := time.Date(2026, 7, 11, 10, 12, 0, 0, time.UTC)
	succeeding := metadata.Operation{
		ID:         "op-helper-succeed",
		Kind:       metadata.PullOperation,
		State:      metadata.OperationRunning,
		ResourceID: "docker.io/library/alpine:latest",
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	failing := metadata.Operation{
		ID:         "op-helper-fail",
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-helper-fail",
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	container := metadata.Container{
		ID:          failing.ResourceID,
		OperationID: failing.ID,
		ImageDigest: "sha256:helper",
		ImageRef:    "docker.io/library/alpine:latest",
		BundlePath:  "/tmp/chamber/bundles/container-helper-fail",
		Runtime:     "runc",
		State:       metadata.ContainerStarting,
		CreatedAt:   startedAt,
		UpdatedAt:   startedAt,
	}
	if err := store.CreateOperation(ctx, succeeding); err != nil {
		t.Fatalf("CreateOperation(succeeding) error = %v", err)
	}
	if err := store.CreateOperation(ctx, failing); err != nil {
		t.Fatalf("CreateOperation(failing) error = %v", err)
	}
	if err := store.CreateContainer(ctx, container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}

	succeeded, err := store.SucceedOperation(ctx, succeeding.ID)
	if err != nil {
		t.Fatalf("SucceedOperation() error = %v", err)
	}
	if succeeded.State != metadata.OperationSucceeded || succeeded.FinishedAt == nil {
		t.Fatalf("SucceedOperation() = %#v, want succeeded with FinishedAt", succeeded)
	}

	failedContainer, failedOperation, err := store.FailContainerAndOperation(ctx, container.ID, metadata.ContainerStarting, failing.ID, chamberErrors.ErrRuntimeStartFailed)
	if err != nil {
		t.Fatalf("FailContainerAndOperation() error = %v", err)
	}
	if failedContainer.State != metadata.ContainerFailed || failedContainer.ErrorCode != chamberErrors.ErrRuntimeStartFailed {
		t.Fatalf("FailContainerAndOperation() container = %#v, want failed runtime_start_failed", failedContainer)
	}
	if failedOperation.State != metadata.OperationFailed || failedOperation.ErrorCode != chamberErrors.ErrRuntimeStartFailed {
		t.Fatalf("FailContainerAndOperation() operation = %#v, want failed runtime_start_failed", failedOperation)
	}
}

func assertContainerLifecycle(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	createdAt := time.Date(2026, 7, 11, 10, 10, 0, 0, time.UTC)
	exitedAt := createdAt.Add(5 * time.Second)
	exitCode := 7
	container := metadata.Container{
		ID:          "container-1",
		OperationID: "op-1",
		TraceID:     "trace-1",
		SpanID:      "span-1",
		ImageDigest: "sha256:abc123",
		ImageRef:    "docker.io/library/alpine:latest",
		BundlePath:  "/tmp/chamber/bundles/container-1",
		Runtime:     "runc",
		State:       metadata.ContainerCreating,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}

	if err := store.CreateContainer(ctx, container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}
	if err := store.CreateContainer(ctx, container); !errors.Is(err, metadata.ErrAlreadyExists) {
		t.Fatalf("CreateContainer(duplicate) error = %v, want %v", err, metadata.ErrAlreadyExists)
	}

	got, err := store.GetContainer(ctx, container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if got != container {
		t.Fatalf("GetContainer() = %#v, want %#v", got, container)
	}

	second := container
	second.ID = "container-0"
	second.OperationID = "op-0"
	second.BundlePath = "/tmp/chamber/bundles/container-0"
	if err := store.CreateContainer(ctx, second); err != nil {
		t.Fatalf("CreateContainer(second) error = %v", err)
	}
	containers, err := store.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers() error = %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("ListContainers() len = %d, want 2; containers = %#v", len(containers), containers)
	}
	if containers[0].ID != second.ID || containers[1].ID != container.ID {
		t.Fatalf("ListContainers() IDs = %q, %q; want sorted IDs %q, %q", containers[0].ID, containers[1].ID, second.ID, container.ID)
	}
	containers[0].ExitCode = &exitCode
	rereadList, err := store.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers(after caller mutation) error = %v", err)
	}
	if rereadList[0].ExitCode != nil {
		t.Fatalf("ListContainers(after caller mutation) ExitCode = %v, want nil", rereadList[0].ExitCode)
	}

	deleted, err := store.DeleteContainer(ctx, second.ID)
	if err != nil {
		t.Fatalf("DeleteContainer() error = %v", err)
	}
	if deleted.ID != second.ID {
		t.Fatalf("DeleteContainer() ID = %q, want %q", deleted.ID, second.ID)
	}
	if _, err := store.GetContainer(ctx, second.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetContainer(deleted) error = %v, want %v", err, metadata.ErrNotFound)
	}
	if _, err := store.DeleteContainer(ctx, second.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("DeleteContainer(deleted) error = %v, want %v", err, metadata.ErrNotFound)
	}

	updated, err := store.TransitionContainer(ctx, container.ID, metadata.ContainerCreating, metadata.ContainerUpdate{
		OperationID: "op-start",
		State:       metadata.ContainerCreated,
		At:          exitedAt,
	})
	if err != nil {
		t.Fatalf("TransitionContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerCreated {
		t.Fatalf("TransitionContainer() state = %q, want %q", updated.State, metadata.ContainerCreated)
	}
	if updated.OperationID != "op-start" {
		t.Fatalf("TransitionContainer() OperationID = %q, want op-start", updated.OperationID)
	}

	reread, err := store.GetContainer(ctx, container.ID)
	if err != nil {
		t.Fatalf("GetContainer(after caller mutation) error = %v", err)
	}
	if reread.OperationID != "op-start" {
		t.Fatalf("GetContainer(after caller mutation) OperationID = %q, want op-start", reread.OperationID)
	}

	_, err = store.TransitionContainer(ctx, container.ID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State: metadata.ContainerRunning,
		At:    exitedAt.Add(time.Second),
	})
	if !errors.Is(err, chamberErrors.ErrStateConflict) {
		t.Fatalf("TransitionContainer(stale from) error = %v, want %v", err, chamberErrors.ErrStateConflict)
	}

	updated, err = store.TransitionContainer(ctx, container.ID, metadata.ContainerCreated, metadata.ContainerUpdate{
		State:    metadata.ContainerFailed,
		At:       exitedAt.Add(2 * time.Second),
		ExitCode: &exitCode,
	})
	if err != nil {
		t.Fatalf("TransitionContainer(created to failed) error = %v", err)
	}
	if updated.ExitCode == nil || *updated.ExitCode != exitCode {
		t.Fatalf("TransitionContainer() ExitCode = %v, want %d", updated.ExitCode, exitCode)
	}

	*updated.ExitCode = 0
	reread, err = store.GetContainer(ctx, container.ID)
	if err != nil {
		t.Fatalf("GetContainer(after caller mutation) error = %v", err)
	}
	if reread.ExitCode == nil || *reread.ExitCode != exitCode {
		t.Fatalf("GetContainer(after caller mutation) ExitCode = %v, want %d", reread.ExitCode, exitCode)
	}

	_, err = store.TransitionContainer(ctx, container.ID, metadata.ContainerFailed, metadata.ContainerUpdate{
		State: metadata.ContainerRunning,
		At:    exitedAt.Add(time.Second),
	})
	if !errors.Is(err, chamberErrors.ErrStateConflict) {
		t.Fatalf("TransitionContainer(invalid transition) error = %v, want %v", err, chamberErrors.ErrStateConflict)
	}
}

func assertConcurrentOperationCreate(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	startedAt := time.Date(2026, 7, 11, 10, 15, 0, 0, time.UTC)
	operation := metadata.Operation{
		ID:         "op-concurrent-create",
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-concurrent-create",
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}

	errs := runConcurrently(20, func(int) error {
		return store.CreateOperation(ctx, operation)
	})

	assertOneSuccess(t, errs, metadata.ErrAlreadyExists)
}

func assertConcurrentOperationTransition(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	startedAt := time.Date(2026, 7, 11, 10, 20, 0, 0, time.UTC)
	operation := metadata.Operation{
		ID:         "op-concurrent-transition",
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-concurrent-transition",
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := store.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}

	errs := runConcurrently(20, func(worker int) error {
		_, err := store.TransitionOperation(ctx, operation.ID, metadata.OperationRunning, metadata.OperationUpdate{
			State: metadata.OperationSucceeded,
			At:    startedAt.Add(time.Duration(worker+1) * time.Second),
		})
		return err
	})

	assertOneSuccess(t, errs, chamberErrors.ErrStateConflict)

	got, err := store.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if got.State != metadata.OperationSucceeded {
		t.Fatalf("GetOperation() state = %q, want %q", got.State, metadata.OperationSucceeded)
	}
	if got.FinishedAt == nil {
		t.Fatal("GetOperation() FinishedAt = nil, want successful transition timestamp")
	}
}

func assertConcurrentTerminalOperationTransition(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	startedAt := time.Date(2026, 7, 11, 10, 22, 0, 0, time.UTC)
	operation := metadata.Operation{
		ID:         "op-concurrent-terminal-transition",
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-concurrent-terminal-transition",
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := store.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}

	terminalStates := []metadata.OperationState{
		metadata.OperationSucceeded,
		metadata.OperationFailed,
	}
	errs := runConcurrently(len(terminalStates), func(worker int) error {
		_, err := store.TransitionOperation(ctx, operation.ID, metadata.OperationRunning, metadata.OperationUpdate{
			State: terminalStates[worker],
			At:    startedAt.Add(time.Duration(worker+1) * time.Second),
		})
		return err
	})

	assertOneSuccess(t, errs, chamberErrors.ErrStateConflict)

	got, err := store.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if got.State != metadata.OperationSucceeded && got.State != metadata.OperationFailed {
		t.Fatalf("GetOperation() state = %q, want one terminal update", got.State)
	}
}

func assertConcurrentContainerTransition(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	createdAt := time.Date(2026, 7, 11, 10, 25, 0, 0, time.UTC)
	container := metadata.Container{
		ID:          "container-concurrent-transition",
		OperationID: "op-concurrent-transition",
		ImageDigest: "sha256:concurrent",
		ImageRef:    "docker.io/library/alpine:latest",
		BundlePath:  "/tmp/chamber/bundles/container-concurrent-transition",
		Runtime:     "runc",
		State:       metadata.ContainerRunning,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
	if err := store.CreateContainer(ctx, container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}

	errs := runConcurrently(20, func(worker int) error {
		exitCode := worker
		_, err := store.TransitionContainer(ctx, container.ID, metadata.ContainerRunning, metadata.ContainerUpdate{
			State:    metadata.ContainerExited,
			At:       createdAt.Add(time.Duration(worker+1) * time.Second),
			ExitCode: &exitCode,
		})
		return err
	})

	assertOneSuccess(t, errs, chamberErrors.ErrStateConflict)

	got, err := store.GetContainer(ctx, container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if got.State != metadata.ContainerExited {
		t.Fatalf("GetContainer() state = %q, want %q", got.State, metadata.ContainerExited)
	}
	if got.ExitCode == nil {
		t.Fatal("GetContainer() ExitCode = nil, want successful transition exit code")
	}
}

func assertTraceFieldsAreExplicit(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.WithValue(context.Background(), traceContextKey{}, "trace-from-context")
	startedAt := time.Date(2026, 7, 11, 10, 30, 0, 0, time.UTC)
	operation := metadata.Operation{
		ID:         "op-explicit-trace",
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-explicit-trace",
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	container := metadata.Container{
		ID:          "container-explicit-trace",
		OperationID: operation.ID,
		ImageDigest: "sha256:trace",
		ImageRef:    "docker.io/library/alpine:latest",
		BundlePath:  "/tmp/chamber/bundles/container-explicit-trace",
		Runtime:     "runc",
		State:       metadata.ContainerCreating,
		CreatedAt:   startedAt,
		UpdatedAt:   startedAt,
	}

	if err := store.CreateOperation(ctx, operation); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}
	if err := store.CreateContainer(ctx, container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}

	gotOperation, err := store.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if gotOperation.TraceID != "" || gotOperation.SpanID != "" {
		t.Fatalf("GetOperation() trace fields = (%q, %q), want empty explicit fields", gotOperation.TraceID, gotOperation.SpanID)
	}

	gotContainer, err := store.GetContainer(ctx, container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if gotContainer.TraceID != "" || gotContainer.SpanID != "" {
		t.Fatalf("GetContainer() trace fields = (%q, %q), want empty explicit fields", gotContainer.TraceID, gotContainer.SpanID)
	}
}

func runConcurrently(count int, fn func(worker int) error) []error {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, count)

	wg.Add(count)
	for i := range count {
		go func(worker int) {
			defer wg.Done()
			<-start
			errs[worker] = fn(worker)
		}(i)
	}

	close(start)
	wg.Wait()
	return errs
}

func assertOneSuccess(t *testing.T, errs []error, conflict error) {
	t.Helper()

	successes := 0
	conflicts := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, conflict):
			conflicts++
		default:
			t.Fatalf("concurrent operation error = %v, want nil or %v", err, conflict)
		}
	}

	if successes != 1 {
		t.Fatalf("successful concurrent operations = %d, want 1", successes)
	}
	if conflicts != len(errs)-1 {
		t.Fatalf("conflicting concurrent operations = %d, want %d", conflicts, len(errs)-1)
	}
}
