package metadata_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	metadataetcd "github.com/donglin-wang/chamber/daemon/metadata/etcd"
	"github.com/donglin-wang/chamber/daemon/metadata/memory"
	"github.com/donglin-wang/chamber/internal/shared/localfs"
)

type traceContextKey struct{}

func TestContainerValidTransition(t *testing.T) {
	tests := map[metadata.StateTransition[metadata.ContainerState]]bool{
		{From: metadata.ContainerCreating, To: metadata.ContainerStarting}: true,
		{From: metadata.ContainerCreating, To: metadata.ContainerFailed}:   true,
		{From: metadata.ContainerStarting, To: metadata.ContainerRunning}:  true,
		{From: metadata.ContainerStarting, To: metadata.ContainerFailed}:   true,
		{From: metadata.ContainerStarting, To: metadata.ContainerExited}:   true,
		{From: metadata.ContainerRunning, To: metadata.ContainerExited}:    true,
		{From: metadata.ContainerRunning, To: metadata.ContainerFailed}:    true,

		{From: metadata.ContainerCreating, To: metadata.ContainerRunning}:       false,
		{From: metadata.ContainerRunning, To: metadata.ContainerStarting}:       false,
		{From: metadata.ContainerExited, To: metadata.ContainerRunning}:         false,
		{From: metadata.ContainerFailed, To: metadata.ContainerRunning}:         false,
		{From: metadata.ContainerRunning, To: metadata.ContainerRunning}:        false,
		{From: metadata.ContainerState("weird"), To: metadata.ContainerRunning}: false,
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
			dataDir, err := os.MkdirTemp("/tmp", "chamber-etcd-*")
			if err != nil {
				t.Fatalf("MkdirTemp() error = %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dataDir) })

			store, err := metadataetcd.Open(ctx, metadata.Config{
				Root: dataDir,
			}, localfs.NewDirectoryManager())
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
			assertTerminalHelpers(t, store)
			assertConcurrentOperationCreate(t, store)
			assertConcurrentOperationTransition(t, store)
			assertConcurrentTerminalOperationTransition(t, store)
			assertConcurrentContainerTransition(t, store)
			assertTraceFieldsAreExplicit(t, store)
		})
	}
}

func assertImageRoundTrip(t *testing.T, store metadata.Store) {
	t.Helper()

	ctx := context.Background()
	pulledAt := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	image := metadata.Image{
		Reference:  "docker.io/library/alpine:latest",
		Digest:     "sha256:abc123",
		LayoutPath: "/tmp/chamber/images/alpine",
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
	if !errors.Is(err, metadata.ErrStateConflict) {
		t.Fatalf("TransitionOperation(stale from) error = %v, want %v", err, metadata.ErrStateConflict)
	}

	_, err = store.TransitionOperation(ctx, operation.ID, metadata.OperationSucceeded, metadata.OperationUpdate{
		State: metadata.OperationFailed,
		At:    finishedAt.Add(time.Second),
	})
	if !errors.Is(err, metadata.ErrStateConflict) {
		t.Fatalf("TransitionOperation(invalid transition) error = %v, want %v", err, metadata.ErrStateConflict)
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

	failedContainer, failedOperation, err := store.FailContainerAndOperation(ctx, container.ID, metadata.ContainerStarting, failing.ID, metadata.ErrRuntimeStartFailed)
	if err != nil {
		t.Fatalf("FailContainerAndOperation() error = %v", err)
	}
	if failedContainer.State != metadata.ContainerFailed || failedContainer.ErrorCode != metadata.ErrRuntimeStartFailed {
		t.Fatalf("FailContainerAndOperation() container = %#v, want failed runtime_start_failed", failedContainer)
	}
	if failedOperation.State != metadata.OperationFailed || failedOperation.ErrorCode != metadata.ErrRuntimeStartFailed {
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

	updated, err := store.TransitionContainer(ctx, container.ID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State:    metadata.ContainerFailed,
		At:       exitedAt,
		ExitCode: &exitCode,
	})
	if err != nil {
		t.Fatalf("TransitionContainer() error = %v", err)
	}
	if updated.State != metadata.ContainerFailed {
		t.Fatalf("TransitionContainer() state = %q, want %q", updated.State, metadata.ContainerFailed)
	}
	if updated.ExitCode == nil || *updated.ExitCode != exitCode {
		t.Fatalf("TransitionContainer() ExitCode = %v, want %d", updated.ExitCode, exitCode)
	}

	*updated.ExitCode = 0
	reread, err := store.GetContainer(ctx, container.ID)
	if err != nil {
		t.Fatalf("GetContainer(after caller mutation) error = %v", err)
	}
	if reread.ExitCode == nil || *reread.ExitCode != exitCode {
		t.Fatalf("GetContainer(after caller mutation) ExitCode = %v, want %d", reread.ExitCode, exitCode)
	}

	_, err = store.TransitionContainer(ctx, container.ID, metadata.ContainerCreating, metadata.ContainerUpdate{
		State: metadata.ContainerRunning,
		At:    exitedAt.Add(time.Second),
	})
	if !errors.Is(err, metadata.ErrStateConflict) {
		t.Fatalf("TransitionContainer(stale from) error = %v, want %v", err, metadata.ErrStateConflict)
	}

	_, err = store.TransitionContainer(ctx, container.ID, metadata.ContainerFailed, metadata.ContainerUpdate{
		State: metadata.ContainerRunning,
		At:    exitedAt.Add(time.Second),
	})
	if !errors.Is(err, metadata.ErrStateConflict) {
		t.Fatalf("TransitionContainer(invalid transition) error = %v, want %v", err, metadata.ErrStateConflict)
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

	assertOneSuccess(t, errs, metadata.ErrStateConflict)

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

	assertOneSuccess(t, errs, metadata.ErrStateConflict)

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

	assertOneSuccess(t, errs, metadata.ErrStateConflict)

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
