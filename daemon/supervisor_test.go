package main

import (
	"context"
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

func TestReconcileSupervisorAppliesCompletedPhase(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainerWithPID(t, store, metadata.ContainerRunning, time.Now().UTC(), 4321)
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

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
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
	if updated.SupervisorPID != 0 || updated.SupervisorStartTime != 0 {
		t.Fatalf("terminal supervisor identity = (%d,%d), want cleared", updated.SupervisorPID, updated.SupervisorStartTime)
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

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v, want classified failed record", err)
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
	state.SupervisorPID = os.Getpid()
	state.SupervisorStartTime, _ = processStartTime(os.Getpid())
	state.StartedAt = &startedAt
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
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

func TestReconcileSupervisorRecordsStartedPhaseFromCreated(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreated, time.Now().UTC())
	state := supervisorStateForContainer(t, container, supervisorStarted)
	startedAt := time.Now().UTC()
	state.RuntimeName = container.Runtime
	state.SupervisorPID = os.Getpid()
	state.SupervisorStartTime, _ = processStartTime(os.Getpid())
	state.StartedAt = &startedAt
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
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

func TestReconcileSupervisorFailsStartedContainerWhenSupervisorProcessExited(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainerWithPID(t, store, metadata.ContainerRunning, time.Now().UTC(), 4321)
	state := supervisorStateForContainer(t, container, supervisorStarted)
	startedAt := time.Now().UTC()
	state.RuntimeName = container.Runtime
	state.SupervisorPID = container.SupervisorPID
	state.SupervisorStartTime = container.SupervisorStartTime
	state.StartedAt = &startedAt
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}
	previous := supervisorProcessMatches
	supervisorProcessMatches = func(pid int, startTime uint64) (bool, error) {
		if pid != container.SupervisorPID {
			t.Fatalf("supervisor PID = %d, want %d", pid, container.SupervisorPID)
		}
		if startTime != container.SupervisorStartTime {
			t.Fatalf("supervisor start time = %d, want %d", startTime, container.SupervisorStartTime)
		}
		return false, nil
	}
	t.Cleanup(func() { supervisorProcessMatches = previous })

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v, want normal failed-record recovery", err)
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
	operation, err := store.GetOperation(context.Background(), container.OperationID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if operation.State != metadata.OperationFailed || operation.ErrorCode != chamberErrors.ErrRuntimeWaitFailed {
		t.Fatalf("operation = %#v, want failed runtime wait", operation)
	}
}

func TestStartupReconciliationFailsNonterminalContainersWithoutSupervisorEvidence(t *testing.T) {
	for _, state := range []metadata.ContainerState{
		metadata.ContainerCreated,
		metadata.ContainerStarting,
		metadata.ContainerRunning,
	} {
		for _, emptyPath := range []bool{false, true} {
			name := string(state) + "-missing-file"
			if emptyPath {
				name = string(state) + "-empty-path"
			}
			t.Run(name, func(t *testing.T) {
				store := memory.NewMemoryStore()
				now := time.Now().UTC()
				operation := metadata.Operation{
					ID: "operation-" + name, Kind: metadata.RunOperation, State: metadata.OperationRunning,
					ResourceID: "container-" + name, StartedAt: now, UpdatedAt: now,
				}
				if err := store.CreateOperation(context.Background(), operation); err != nil {
					t.Fatalf("CreateOperation() error = %v", err)
				}
				supervisorPath := filepath.Join(t.TempDir(), "missing", "supervisor.json")
				if emptyPath {
					supervisorPath = ""
				}
				container := metadata.Container{
					ID: operation.ResourceID, OperationID: operation.ID, State: state,
					SupervisorPath: supervisorPath, CreatedAt: now, UpdatedAt: now,
				}
				if err := store.CreateContainer(context.Background(), container); err != nil {
					t.Fatalf("CreateContainer() error = %v", err)
				}

				if err := reconcileSupervisorContainer(context.Background(), store, container, nil, nil, chamberRuntime.Config{}, true); err != nil {
					t.Fatalf("startup reconciliation error = %v, want normal classified recovery", err)
				}
				updated, err := store.GetContainer(context.Background(), container.ID)
				if err != nil {
					t.Fatalf("GetContainer() error = %v", err)
				}
				if updated.State != metadata.ContainerFailed || updated.ErrorCode != chamberErrors.ErrRuntimeStartFailed {
					t.Fatalf("container = %#v, want failed runtime_start_failed", updated)
				}
				updatedOperation, err := store.GetOperation(context.Background(), operation.ID)
				if err != nil {
					t.Fatalf("GetOperation() error = %v", err)
				}
				if updatedOperation.State != metadata.OperationFailed {
					t.Fatalf("operation state = %q, want %q", updatedOperation.State, metadata.OperationFailed)
				}
			})
		}
	}
}

func TestReconcileSupervisorRestartsPreparedRunContainer(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreating, time.Now().UTC().Add(-supervisorCreatingTimeout-time.Second))
	state := supervisorStateForContainer(t, container, supervisorPrepared)
	state.UpdatedAt = time.Now().UTC().Add(-supervisorLaunchTimeout - time.Second)
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v", err)
	}

	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerStarting {
		t.Fatalf("container state = %q, want %q", updated.State, metadata.ContainerStarting)
	}
	if updated.SupervisorPID != 1234 {
		t.Fatalf("supervisor pid = %d, want 1234", updated.SupervisorPID)
	}
}

func TestReconcileSupervisorDoesNotDuplicatePreparedSupervisorRecordedByContainer(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainerWithPID(t, store, metadata.ContainerStarting, time.Now().UTC(), os.Getpid())
	if err := writeSupervisorFile(container.SupervisorPath, supervisorStateForContainer(t, container, supervisorPrepared)); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}
	started := false
	startSupervisor := func(context.Context, string) (int, func() error, error) {
		started = true
		return 0, nil, nil
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container, startSupervisor, fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, false); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v", err)
	}
	if started {
		t.Fatal("reconcileSupervisorContainer() launched a duplicate supervisor")
	}
}

func TestPeriodicReconcileDoesNotClaimFreshPreparedSupervisor(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreated, time.Now().UTC())
	if err := writeSupervisorFile(container.SupervisorPath, supervisorStateForContainer(t, container, supervisorPrepared)); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}
	started := false
	startSupervisor := func(context.Context, string) (int, func() error, error) {
		started = true
		return 0, nil, nil
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container, startSupervisor, fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, false); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v", err)
	}
	if started {
		t.Fatal("periodic reconcile claimed a freshly prepared supervisor")
	}
}

func TestReconcileSupervisorAdoptsStartOperationAfterDaemonCrash(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreated, time.Now().UTC())
	startOperation := metadata.Operation{
		ID:         "operation-recovered-start",
		Kind:       metadata.StartOperation,
		State:      metadata.OperationRunning,
		ResourceID: container.ID,
		StartedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	if err := store.CreateOperation(context.Background(), startOperation); err != nil {
		t.Fatalf("CreateOperation(start) error = %v", err)
	}
	state := supervisorStateForContainer(t, container, supervisorPrepared)
	state.OperationID = startOperation.ID
	state.UpdatedAt = time.Now().UTC().Add(-supervisorLaunchTimeout - time.Second)
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v", err)
	}
	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerStarting || updated.OperationID != startOperation.ID || updated.SupervisorPID != 1234 {
		t.Fatalf("recovered container = %#v, want starting with recovered operation and pid", updated)
	}
}

func TestReconcileSupervisorAlignsAtomicallyAdmittedStartBeforeEvidenceRewrite(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreated, time.Now().UTC())
	state := supervisorStateForContainer(t, container, supervisorPrepared)
	state.UpdatedAt = time.Now().UTC().Add(-supervisorLaunchTimeout - time.Second)
	if err := writeSupervisorFile(container.SupervisorPath, state); err != nil {
		t.Fatalf("writeSupervisorFile(create) error = %v", err)
	}
	startOperation := metadata.Operation{
		ID: "operation-atomically-admitted-start", Kind: metadata.StartOperation, State: metadata.OperationRunning,
		ResourceID: container.ID, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	starting, err := store.CreateOperationAndTransitionContainer(
		context.Background(), startOperation, container.ID, metadata.ContainerCreated,
		metadata.ContainerUpdate{OperationID: startOperation.ID, State: metadata.ContainerStarting, At: time.Now().UTC()},
	)
	if err != nil {
		t.Fatalf("CreateOperationAndTransitionContainer() error = %v", err)
	}

	if err := reconcileSupervisorContainer(context.Background(), store, starting, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v", err)
	}
	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.OperationID != startOperation.ID || updated.State != metadata.ContainerStarting || updated.SupervisorPID != 1234 {
		t.Fatalf("recovered container = %#v", updated)
	}
	aligned, err := readRequiredSupervisorFile(container.SupervisorPath)
	if err != nil {
		t.Fatalf("readRequiredSupervisorFile() error = %v", err)
	}
	if aligned.OperationID != startOperation.ID {
		t.Fatalf("supervisor operation = %q, want %q", aligned.OperationID, startOperation.ID)
	}
}

func TestProcessMatchesRejectsReusedPIDIdentity(t *testing.T) {
	startTime, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("processStartTime() error = %v", err)
	}
	matched, err := processMatches(os.Getpid(), startTime)
	if err != nil || !matched {
		t.Fatalf("processMatches(correct) = %v, %v; want true", matched, err)
	}
	matched, err = processMatches(os.Getpid(), startTime+1)
	if err != nil || matched {
		t.Fatalf("processMatches(reused) = %v, %v; want false", matched, err)
	}
}

func TestStartupReconcileClassifiesCreatingRecordAndSchedulesCleanup(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createSupervisorContainer(t, store, metadata.ContainerCreating, time.Now().UTC())

	if err := reconcileSupervisorContainer(context.Background(), store, container, fakeStartSupervisor(nil), fakeOpenContainer(nil), chamberRuntime.Config{Name: "fake"}, true); err != nil {
		t.Fatalf("reconcileSupervisorContainer() error = %v, want normal recovery", err)
	}
	updated, err := store.GetContainer(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("GetContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerFailed || updated.ErrorCode != chamberErrors.ErrRuntimeStartFailed {
		t.Fatalf("recovered container = %#v, want failed runtime start", updated)
	}
	cleanups, err := store.ListCleanups(context.Background())
	if err != nil || len(cleanups) != 1 || cleanups[0].ContainerID != container.ID || cleanups[0].LeaseID != "" {
		t.Fatalf("ListCleanups() = %#v, %v; want one reclaimable cleanup", cleanups, err)
	}
}

func createSupervisorContainer(t *testing.T, store metadata.Store, state metadata.ContainerState, updatedAt time.Time) metadata.Container {
	return createSupervisorContainerWithPID(t, store, state, updatedAt, 0)
}

func createSupervisorContainerWithPID(t *testing.T, store metadata.Store, state metadata.ContainerState, updatedAt time.Time, supervisorPID int) metadata.Container {
	t.Helper()
	supervisorStartTime := uint64(77)
	if supervisorPID == 0 {
		supervisorStartTime = 0
	} else if supervisorPID == os.Getpid() {
		var err error
		supervisorStartTime, err = processStartTime(supervisorPID)
		if err != nil {
			t.Fatalf("processStartTime() error = %v", err)
		}
	}

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
		ID:                  containerID,
		OperationID:         operationID,
		ImageDigest:         "sha256:image",
		ImageRef:            "docker.io/library/alpine:latest",
		BundlePath:          filepath.Join(root, "bundle"),
		StdoutPath:          filepath.Join(root, "stdout.log"),
		StderrPath:          filepath.Join(root, "stderr.log"),
		Runtime:             "fake",
		RuntimeRoot:         filepath.Join(root, "runtime"),
		SupervisorPath:      filepath.Join(root, "supervisor.json"),
		SupervisorPID:       supervisorPID,
		SupervisorStartTime: supervisorStartTime,
		State:               state,
		CreatedAt:           updatedAt,
		UpdatedAt:           updatedAt,
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
