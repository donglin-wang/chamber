package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	chbundle "github.com/donglin-wang/chamber/internal/bundle"
	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/donglin-wang/chamber/internal/metadata"
	chruntime "github.com/donglin-wang/chamber/internal/runtime"
)

func TestPullSuccessCreatesDurableOperationBeforePull(t *testing.T) {
	store := newFakeStore()
	events := newEventLog()
	service := newTestService(t, store, events)
	service.IDs = &sequenceIDs{values: []string{"op-pull"}}
	service.Puller = &fakePuller{
		events: events,
		pulled: chimage.PulledImage{
			Reference:  "docker.io/library/alpine:latest",
			Digest:     "sha256:abc123",
			LayoutPath: filepath.Join(service.ImageRoot, "pulled-layout"),
			PulledAt:   testTime,
		},
	}

	result, err := service.Pull(context.Background(), PullRequest{
		Reference: "docker.io/library/alpine:latest",
	})
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if result.Operation.State != metadata.OperationSucceeded {
		t.Fatalf("Pull() operation state = %q, want %q", result.Operation.State, metadata.OperationSucceeded)
	}
	if result.Image.Digest != "sha256:abc123" {
		t.Fatalf("Pull() image digest = %q, want sha256:abc123", result.Image.Digest)
	}

	wantEvents := []string{
		"create-operation:op-pull:running",
		"pull:docker.io/library/alpine:latest",
		"put-image:docker.io/library/alpine:latest",
		"transition-operation:op-pull:running->succeeded",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %#v, want %#v", got, wantEvents)
	}
}

func TestPullMetadataWriteFailureFailsOperation(t *testing.T) {
	store := newFakeStore()
	store.putImageErr = errors.New("metadata unavailable")
	events := newEventLog()
	service := newTestService(t, store, events)
	service.IDs = &sequenceIDs{values: []string{"op-pull"}}
	service.Puller = &fakePuller{
		events: events,
		pulled: chimage.PulledImage{
			Reference:  "docker.io/library/alpine:latest",
			Digest:     "sha256:abc123",
			LayoutPath: filepath.Join(service.ImageRoot, "pulled-layout"),
			PulledAt:   testTime,
		},
	}

	result, err := service.Pull(context.Background(), PullRequest{
		Reference: "docker.io/library/alpine:latest",
	})
	assertDaemonError(t, err, "op-pull", metadata.ErrMetadataFailed)
	if result.Operation.State != metadata.OperationFailed {
		t.Fatalf("Pull() operation state = %q, want %q", result.Operation.State, metadata.OperationFailed)
	}
	if result.Operation.ErrorCode != metadata.ErrMetadataFailed {
		t.Fatalf("Pull() operation error code = %q, want %q", result.Operation.ErrorCode, metadata.ErrMetadataFailed)
	}

	wantEvents := []string{
		"create-operation:op-pull:running",
		"pull:docker.io/library/alpine:latest",
		"put-image:docker.io/library/alpine:latest",
		"transition-operation:op-pull:running->failed",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %#v, want %#v", got, wantEvents)
	}
}

func TestRunMissingImageFailsOperationWithoutSideEffects(t *testing.T) {
	store := newFakeStore()
	events := newEventLog()
	service := newTestService(t, store, events)
	service.IDs = &sequenceIDs{values: []string{"op-run", "ctr-run"}}

	result, err := service.Run(context.Background(), RunRequest{
		Image:   "docker.io/library/missing:latest",
		Command: []string{"/bin/sh"},
	})
	assertDaemonError(t, err, "op-run", metadata.ErrImageNotFound)
	if result.Operation.State != metadata.OperationFailed {
		t.Fatalf("Run() operation state = %q, want %q", result.Operation.State, metadata.OperationFailed)
	}
	if result.Operation.ErrorCode != metadata.ErrImageNotFound {
		t.Fatalf("Run() operation error code = %q, want %q", result.Operation.ErrorCode, metadata.ErrImageNotFound)
	}
	if result.Container.ID != "" {
		t.Fatalf("Run() container = %#v, want zero value", result.Container)
	}

	wantEvents := []string{
		"create-operation:op-run:running",
		"get-image:docker.io/library/missing:latest",
		"transition-operation:op-run:running->failed",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %#v, want %#v", got, wantEvents)
	}
}

func TestRunProvisionFailureFailsContainerAndOperation(t *testing.T) {
	store := newFakeStore()
	events := newEventLog()
	service := newTestService(t, store, events)
	service.IDs = &sequenceIDs{values: []string{"op-run", "ctr-run"}}
	store.images["docker.io/library/alpine:latest"] = testImage(service.ImageRoot)
	service.Provisioner = &fakeProvisioner{
		events: events,
		err:    errors.New("provision failed"),
	}

	result, err := service.Run(context.Background(), RunRequest{
		Image:   "docker.io/library/alpine:latest",
		Command: []string{"/bin/sh"},
	})
	assertDaemonError(t, err, "op-run", metadata.ErrBundlePrepareFailed)
	if result.Container.State != metadata.ContainerFailed {
		t.Fatalf("Run() container state = %q, want %q", result.Container.State, metadata.ContainerFailed)
	}
	if result.Container.ErrorCode != metadata.ErrBundlePrepareFailed {
		t.Fatalf("Run() container error code = %q, want %q", result.Container.ErrorCode, metadata.ErrBundlePrepareFailed)
	}
	if result.Operation.State != metadata.OperationFailed {
		t.Fatalf("Run() operation state = %q, want %q", result.Operation.State, metadata.OperationFailed)
	}

	wantEvents := []string{
		"create-operation:op-run:running",
		"get-image:docker.io/library/alpine:latest",
		"create-container:ctr-run:creating",
		"provision:ctr-run",
		"transition-container:ctr-run:creating->failed",
		"transition-operation:op-run:running->failed",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %#v, want %#v", got, wantEvents)
	}
}

func TestRunRuntimeStartFailureFailsContainerAndOperation(t *testing.T) {
	store := newFakeStore()
	events := newEventLog()
	service := newTestService(t, store, events)
	service.IDs = &sequenceIDs{values: []string{"op-run", "ctr-run"}}
	store.images["docker.io/library/alpine:latest"] = testImage(service.ImageRoot)
	service.Runtime = &fakeRuntime{
		events: events,
		err:    errors.New("runtime start failed"),
	}

	result, err := service.Run(context.Background(), RunRequest{
		Image:   "docker.io/library/alpine:latest",
		Command: []string{"/bin/sh"},
	})
	assertDaemonError(t, err, "op-run", metadata.ErrRuntimeStartFailed)
	if result.Container.State != metadata.ContainerFailed {
		t.Fatalf("Run() container state = %q, want %q", result.Container.State, metadata.ContainerFailed)
	}
	if result.Operation.State != metadata.OperationFailed {
		t.Fatalf("Run() operation state = %q, want %q", result.Operation.State, metadata.OperationFailed)
	}
	if got := service.Runtime.(*fakeRuntime).gotLifetimeValue; got != "daemon-lifetime" {
		t.Fatalf("Runtime.Run() lifetime value = %q, want daemon-lifetime", got)
	}

	wantEvents := []string{
		"create-operation:op-run:running",
		"get-image:docker.io/library/alpine:latest",
		"create-container:ctr-run:creating",
		"provision:ctr-run",
		"transition-container:ctr-run:creating->starting",
		"runtime-run:ctr-run",
		"transition-container:ctr-run:starting->failed",
		"transition-operation:op-run:running->failed",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %#v, want %#v", got, wantEvents)
	}
}

func TestRunRunningProcessEventuallyExitsZero(t *testing.T) {
	store := newFakeStore()
	events := newEventLog()
	service := newTestService(t, store, events)
	service.IDs = &sequenceIDs{values: []string{"op-run", "ctr-run"}}
	store.images["docker.io/library/alpine:latest"] = testImage(service.ImageRoot)
	process := newFakeProcess()
	service.Runtime = &fakeRuntime{
		events: events,
		result: chruntime.StartResult{
			Process: process,
			State:   chruntime.ProcessRunning,
		},
	}

	result, err := service.Run(context.Background(), RunRequest{
		Image:   "docker.io/library/alpine:latest",
		Command: []string{"/bin/sh"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Operation.State != metadata.OperationRunning {
		t.Fatalf("Run() operation state = %q, want %q", result.Operation.State, metadata.OperationRunning)
	}
	if result.Container.State != metadata.ContainerRunning {
		t.Fatalf("Run() container state = %q, want %q", result.Container.State, metadata.ContainerRunning)
	}

	process.finish(0, nil)
	operation := store.waitOperationState(t, "op-run", metadata.OperationSucceeded)
	container := store.waitContainerState(t, "ctr-run", metadata.ContainerExited)
	if operation.ErrorCode != "" {
		t.Fatalf("operation error code = %q, want empty", operation.ErrorCode)
	}
	if container.ExitCode == nil || *container.ExitCode != 0 {
		t.Fatalf("container exit code = %v, want 0", container.ExitCode)
	}
}

func TestRunRunningProcessEventuallyExitsNonzero(t *testing.T) {
	store := newFakeStore()
	events := newEventLog()
	service := newTestService(t, store, events)
	service.IDs = &sequenceIDs{values: []string{"op-run", "ctr-run"}}
	store.images["docker.io/library/alpine:latest"] = testImage(service.ImageRoot)
	process := newFakeProcess()
	service.Runtime = &fakeRuntime{
		events: events,
		result: chruntime.StartResult{
			Process: process,
			State:   chruntime.ProcessRunning,
		},
	}

	result, err := service.Run(context.Background(), RunRequest{
		Image:   "docker.io/library/alpine:latest",
		Command: []string{"/bin/sh"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Container.State != metadata.ContainerRunning {
		t.Fatalf("Run() container state = %q, want %q", result.Container.State, metadata.ContainerRunning)
	}

	process.finish(7, nil)
	operation := store.waitOperationState(t, "op-run", metadata.OperationFailed)
	container := store.waitContainerState(t, "ctr-run", metadata.ContainerExited)
	if operation.ErrorCode != metadata.ErrContainerExitNonzero {
		t.Fatalf("operation error code = %q, want %q", operation.ErrorCode, metadata.ErrContainerExitNonzero)
	}
	if container.ErrorCode != metadata.ErrContainerExitNonzero {
		t.Fatalf("container error code = %q, want %q", container.ErrorCode, metadata.ErrContainerExitNonzero)
	}
	if container.ExitCode == nil || *container.ExitCode != 7 {
		t.Fatalf("container exit code = %v, want 7", container.ExitCode)
	}
}

var testTime = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

type contextKey string

func newTestService(t *testing.T, store *fakeStore, events *eventLog) *Service {
	t.Helper()

	root := t.TempDir()
	lifetime := context.WithValue(context.Background(), contextKey("lifetime"), "daemon-lifetime")
	service := &Service{
		Store:         store,
		Puller:        &fakePuller{events: events},
		Runtime:       &fakeRuntime{events: events},
		Binary:        chruntime.Binary{Name: "runc", Version: "test", Path: "/bin/runc"},
		Provisioner:   &fakeProvisioner{events: events},
		Clock:         fixedClock{now: testTime},
		IDs:           &sequenceIDs{values: []string{"op", "ctr"}},
		Lifetime:      lifetime,
		ImageRoot:     filepath.Join(root, "images"),
		RuntimeRoot:   filepath.Join(root, "runtime"),
		ContainerRoot: filepath.Join(root, "containers"),
		Platform:      "linux/amd64",
	}
	store.events = events
	return service
}

func testImage(root string) metadata.Image {
	return metadata.Image{
		Reference:  "docker.io/library/alpine:latest",
		Digest:     "sha256:abc123",
		LayoutPath: filepath.Join(root, "alpine-layout"),
		PulledAt:   testTime,
		LastUsedAt: testTime,
	}
}

func assertDaemonError(t *testing.T, err error, operationID string, code metadata.ErrorCode) {
	t.Helper()

	var daemonErr *Error
	if !errors.As(err, &daemonErr) {
		t.Fatalf("error = %v, want *daemon.Error", err)
	}
	if daemonErr.OperationID != operationID {
		t.Fatalf("OperationID = %q, want %q", daemonErr.OperationID, operationID)
	}
	if daemonErr.Code != string(code) {
		t.Fatalf("Code = %q, want %q", daemonErr.Code, code)
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func newEventLog() *eventLog {
	return &eventLog{}
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

type sequenceIDs struct {
	next   int
	values []string
}

func (g *sequenceIDs) New() string {
	if g.next >= len(g.values) {
		panic("sequenceIDs exhausted")
	}
	id := g.values[g.next]
	g.next++
	return id
}

type fakePuller struct {
	events *eventLog
	pulled chimage.PulledImage
	err    error
}

func (p *fakePuller) Pull(ctx context.Context, request chimage.PullRequest) (chimage.PulledImage, error) {
	if err := ctx.Err(); err != nil {
		return chimage.PulledImage{}, err
	}
	p.events.add("pull:" + request.Reference)
	if p.err != nil {
		return chimage.PulledImage{}, p.err
	}
	if p.pulled.Reference == "" {
		p.pulled = chimage.PulledImage{
			Reference:  request.Reference,
			Digest:     "sha256:abc123",
			LayoutPath: request.Destination,
			PulledAt:   testTime,
		}
	}
	return p.pulled, nil
}

type fakeProvisioner struct {
	events *eventLog
	bundle chbundle.ProvisionedBundle
	err    error
}

func (p *fakeProvisioner) Provision(ctx context.Context, request chbundle.ProvisionRequest) (chbundle.ProvisionedBundle, error) {
	if err := ctx.Err(); err != nil {
		return chbundle.ProvisionedBundle{}, err
	}
	p.events.add("provision:" + request.ContainerID)
	if p.err != nil {
		return chbundle.ProvisionedBundle{}, p.err
	}
	if p.bundle.ContainerID == "" {
		p.bundle.ContainerID = request.ContainerID
	}
	return p.bundle, nil
}

type fakeRuntime struct {
	events           *eventLog
	result           chruntime.StartResult
	err              error
	gotLifetimeValue string
}

func (r *fakeRuntime) Ensure(ctx context.Context) (chruntime.Binary, error) {
	return chruntime.Binary{Name: "runc", Version: "test", Path: "/bin/runc"}, nil
}

func (r *fakeRuntime) Run(ctx context.Context, binary chruntime.Binary, request chruntime.RunRequest) (chruntime.StartResult, error) {
	r.events.add("runtime-run:" + request.Bundle.ContainerID)
	if value, ok := ctx.Value(contextKey("lifetime")).(string); ok {
		r.gotLifetimeValue = value
	}
	if r.err != nil {
		return chruntime.StartResult{}, r.err
	}
	if r.result.State == "" {
		r.result = chruntime.StartResult{
			Process: completedProcess{exitCode: 0},
			State:   chruntime.ProcessExited,
		}
	}
	return r.result, nil
}

type completedProcess struct {
	exitCode int
	err      error
}

func (p completedProcess) Wait() (int, error) {
	return p.exitCode, p.err
}

type fakeProcess struct {
	done chan waitResult
}

type waitResult struct {
	exitCode int
	err      error
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{done: make(chan waitResult, 1)}
}

func (p *fakeProcess) finish(exitCode int, err error) {
	p.done <- waitResult{exitCode: exitCode, err: err}
}

func (p *fakeProcess) Wait() (int, error) {
	result := <-p.done
	return result.exitCode, result.err
}

type fakeStore struct {
	mu          sync.Mutex
	events      *eventLog
	images      map[string]metadata.Image
	operations  map[string]metadata.Operation
	containers  map[string]metadata.Container
	putImageErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		images:     make(map[string]metadata.Image),
		operations: make(map[string]metadata.Operation),
		containers: make(map[string]metadata.Container),
	}
}

func (s *fakeStore) PutImage(ctx context.Context, image metadata.Image) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.add("put-image:" + image.Reference)
	if s.putImageErr != nil {
		return s.putImageErr
	}
	s.images[image.Reference] = image
	return nil
}

func (s *fakeStore) GetImage(ctx context.Context, reference string) (metadata.Image, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Image{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.add("get-image:" + reference)
	image, ok := s.images[reference]
	if !ok {
		return metadata.Image{}, metadata.ErrNotFound
	}
	return image, nil
}

func (s *fakeStore) CreateOperation(ctx context.Context, operation metadata.Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.add(fmt.Sprintf("create-operation:%s:%s", operation.ID, operation.State))
	if _, ok := s.operations[operation.ID]; ok {
		return metadata.ErrAlreadyExists
	}
	s.operations[operation.ID] = cloneOperation(operation)
	return nil
}

func (s *fakeStore) GetOperation(ctx context.Context, id string) (metadata.Operation, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Operation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, ok := s.operations[id]
	if !ok {
		return metadata.Operation{}, metadata.ErrNotFound
	}
	return cloneOperation(operation), nil
}

func (s *fakeStore) TransitionOperation(
	ctx context.Context,
	id string,
	from metadata.OperationState,
	update metadata.OperationUpdate,
) (metadata.Operation, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Operation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.add(fmt.Sprintf("transition-operation:%s:%s->%s", id, from, update.State))
	operation, ok := s.operations[id]
	if !ok {
		return metadata.Operation{}, metadata.ErrNotFound
	}
	if operation.State != from || !metadata.IsOperationTransitionValid(from, update.State) {
		return metadata.Operation{}, metadata.ErrStateConflict
	}
	operation.State = update.State
	operation.UpdatedAt = update.At
	operation.FinishedAt = &update.At
	operation.ErrorCode = metadata.ErrorCode(update.ErrorCode)
	s.operations[id] = cloneOperation(operation)
	return cloneOperation(operation), nil
}

func (s *fakeStore) CreateContainer(ctx context.Context, container metadata.Container) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.add(fmt.Sprintf("create-container:%s:%s", container.ID, container.State))
	if _, ok := s.containers[container.ID]; ok {
		return metadata.ErrAlreadyExists
	}
	s.containers[container.ID] = cloneContainer(container)
	return nil
}

func (s *fakeStore) GetContainer(ctx context.Context, id string) (metadata.Container, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Container{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	container, ok := s.containers[id]
	if !ok {
		return metadata.Container{}, metadata.ErrNotFound
	}
	return cloneContainer(container), nil
}

func (s *fakeStore) TransitionContainer(
	ctx context.Context,
	id string,
	from metadata.ContainerState,
	update metadata.ContainerUpdate,
) (metadata.Container, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Container{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.add(fmt.Sprintf("transition-container:%s:%s->%s", id, from, update.State))
	container, ok := s.containers[id]
	if !ok {
		return metadata.Container{}, metadata.ErrNotFound
	}
	if container.State != from || !metadata.IsContainerTransitionValid(from, update.State) {
		return metadata.Container{}, metadata.ErrStateConflict
	}
	container.State = update.State
	container.UpdatedAt = update.At
	container.ExitCode = cloneInt(update.ExitCode)
	container.ErrorCode = metadata.ErrorCode(update.ErrorCode)
	s.containers[id] = cloneContainer(container)
	return cloneContainer(container), nil
}

func (s *fakeStore) Close() error {
	return nil
}

func (s *fakeStore) waitOperationState(t *testing.T, id string, state metadata.OperationState) metadata.Operation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := s.GetOperation(context.Background(), id)
		if err == nil && operation.State == state {
			return operation
		}
		time.Sleep(10 * time.Millisecond)
	}
	operation, _ := s.GetOperation(context.Background(), id)
	t.Fatalf("operation %s state = %q, want %q", id, operation.State, state)
	return metadata.Operation{}
}

func (s *fakeStore) waitContainerState(t *testing.T, id string, state metadata.ContainerState) metadata.Container {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		container, err := s.GetContainer(context.Background(), id)
		if err == nil && container.State == state {
			return container
		}
		time.Sleep(10 * time.Millisecond)
	}
	container, _ := s.GetContainer(context.Background(), id)
	t.Fatalf("container %s state = %q, want %q", id, container.State, state)
	return metadata.Container{}
}

func cloneOperation(operation metadata.Operation) metadata.Operation {
	if operation.FinishedAt != nil {
		finishedAt := *operation.FinishedAt
		operation.FinishedAt = &finishedAt
	}
	return operation
}

func cloneContainer(container metadata.Container) metadata.Container {
	container.ExitCode = cloneInt(container.ExitCode)
	return container
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

var _ metadata.Store = (*fakeStore)(nil)
