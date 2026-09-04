package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

type MemoryStore struct {
	mu         sync.RWMutex
	images     map[string]metadata.Image
	operations map[string]metadata.Operation
	containers map[string]metadata.Container
	cleanups   map[string]metadata.Cleanup
	closed     bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		images:     make(map[string]metadata.Image),
		operations: make(map[string]metadata.Operation),
		containers: make(map[string]metadata.Container),
		cleanups:   make(map[string]metadata.Cleanup),
	}
}

func (s *MemoryStore) PutImage(ctx context.Context, image metadata.Image) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.ErrNotFound
	}

	s.images[image.Reference] = image
	return nil
}

func (s *MemoryStore) GetImage(ctx context.Context, reference string) (metadata.Image, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Image{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return metadata.Image{}, metadata.ErrNotFound
	}

	image, ok := s.images[reference]
	if !ok {
		return metadata.Image{}, metadata.ErrNotFound
	}
	return image, nil
}

func (s *MemoryStore) CreateOperation(ctx context.Context, operation metadata.Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.ErrNotFound
	}
	if _, ok := s.operations[operation.ID]; ok {
		return metadata.ErrAlreadyExists
	}

	s.operations[operation.ID] = cloneOperation(operation)
	return nil
}

func (s *MemoryStore) GetOperation(ctx context.Context, id string) (metadata.Operation, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Operation{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return metadata.Operation{}, metadata.ErrNotFound
	}

	operation, ok := s.operations[id]
	if !ok {
		return metadata.Operation{}, metadata.ErrNotFound
	}
	return cloneOperation(operation), nil
}

func (s *MemoryStore) ListOperations(ctx context.Context) ([]metadata.Operation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, metadata.ErrNotFound
	}
	operations := make([]metadata.Operation, 0, len(s.operations))
	for _, operation := range s.operations {
		operations = append(operations, cloneOperation(operation))
	}
	sort.Slice(operations, func(i, j int) bool {
		return operations[i].ID < operations[j].ID
	})
	return operations, nil
}

func (s *MemoryStore) SucceedOperation(ctx context.Context, id string) (metadata.Operation, error) {
	return s.TransitionOperation(ctx, id, metadata.OperationRunning, metadata.OperationUpdate{
		State: metadata.OperationSucceeded,
		At:    time.Now().UTC(),
	})
}

func (s *MemoryStore) FailOperation(ctx context.Context, id string, code chamberErrors.Code) (metadata.Operation, error) {
	return s.TransitionOperation(ctx, id, metadata.OperationRunning, metadata.OperationUpdate{
		State:     metadata.OperationFailed,
		At:        time.Now().UTC(),
		ErrorCode: code,
	})
}

func (s *MemoryStore) TransitionOperation(
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
	if s.closed {
		return metadata.Operation{}, metadata.ErrNotFound
	}

	operation, ok := s.operations[id]
	if !ok {
		return metadata.Operation{}, metadata.ErrNotFound
	}
	if operation.State != from {
		return metadata.Operation{}, chamberErrors.ErrStateConflict
	}
	if !metadata.IsOperationTransitionValid(from, update.State) {
		return metadata.Operation{}, chamberErrors.ErrStateConflict
	}

	operation = applyOperationUpdate(operation, update)
	s.operations[id] = cloneOperation(operation)
	return cloneOperation(operation), nil
}

func (s *MemoryStore) CreateContainer(ctx context.Context, container metadata.Container) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.ErrNotFound
	}
	if _, ok := s.containers[container.ID]; ok {
		return metadata.ErrAlreadyExists
	}

	s.containers[container.ID] = cloneContainer(container)
	return nil
}

func (s *MemoryStore) CreateContainerAndOperation(ctx context.Context, container metadata.Container, operation metadata.Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.ErrNotFound
	}
	if _, ok := s.containers[container.ID]; ok {
		return metadata.ErrAlreadyExists
	}
	if _, ok := s.operations[operation.ID]; ok {
		return metadata.ErrAlreadyExists
	}

	s.operations[operation.ID] = cloneOperation(operation)
	s.containers[container.ID] = cloneContainer(container)
	return nil
}

func (s *MemoryStore) CreateOperationAndTransitionContainer(
	ctx context.Context,
	operation metadata.Operation,
	containerID string,
	containerFrom metadata.ContainerState,
	containerUpdate metadata.ContainerUpdate,
) (metadata.Container, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Container{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.Container{}, metadata.ErrNotFound
	}
	if _, ok := s.operations[operation.ID]; ok {
		return metadata.Container{}, metadata.ErrAlreadyExists
	}
	container, ok := s.containers[containerID]
	if !ok {
		return metadata.Container{}, metadata.ErrNotFound
	}
	if container.State != containerFrom || !metadata.IsContainerTransitionValid(containerFrom, containerUpdate.State) {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	container = applyContainerUpdate(container, containerUpdate)
	s.operations[operation.ID] = cloneOperation(operation)
	s.containers[containerID] = cloneContainer(container)
	return cloneContainer(container), nil
}

func (s *MemoryStore) GetContainer(ctx context.Context, id string) (metadata.Container, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Container{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return metadata.Container{}, metadata.ErrNotFound
	}

	container, ok := s.containers[id]
	if !ok {
		return metadata.Container{}, metadata.ErrNotFound
	}
	return cloneContainer(container), nil
}

func (s *MemoryStore) ListContainers(ctx context.Context) ([]metadata.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, metadata.ErrNotFound
	}

	containers := make([]metadata.Container, 0, len(s.containers))
	for _, container := range s.containers {
		containers = append(containers, cloneContainer(container))
	}
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].ID < containers[j].ID
	})
	return containers, nil
}

func (s *MemoryStore) DeleteContainer(ctx context.Context, id string) (metadata.Container, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Container{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.Container{}, metadata.ErrNotFound
	}

	container, ok := s.containers[id]
	if !ok {
		return metadata.Container{}, metadata.ErrNotFound
	}
	delete(s.containers, id)
	return cloneContainer(container), nil
}

func (s *MemoryStore) TransitionContainer(
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
	if s.closed {
		return metadata.Container{}, metadata.ErrNotFound
	}

	container, ok := s.containers[id]
	if !ok {
		return metadata.Container{}, metadata.ErrNotFound
	}
	if container.State != from {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	if !metadata.IsContainerTransitionValid(from, update.State) {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}

	container = applyContainerUpdate(container, update)
	s.containers[id] = cloneContainer(container)
	return cloneContainer(container), nil
}

func (s *MemoryStore) SetContainerSupervisor(ctx context.Context, id string, state metadata.ContainerState, pid int, startTime uint64, at time.Time) (metadata.Container, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Container{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.Container{}, metadata.ErrNotFound
	}
	container, ok := s.containers[id]
	if !ok {
		return metadata.Container{}, metadata.ErrNotFound
	}
	if container.State != state {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	container.SupervisorPID = pid
	container.SupervisorStartTime = startTime
	container.UpdatedAt = at
	s.containers[id] = cloneContainer(container)
	return cloneContainer(container), nil
}

func (s *MemoryStore) TransitionContainerAndOperation(
	ctx context.Context,
	containerID string,
	containerFrom metadata.ContainerState,
	containerUpdate metadata.ContainerUpdate,
	operationID string,
	operationFrom metadata.OperationState,
	operationUpdate metadata.OperationUpdate,
) (metadata.Container, metadata.Operation, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Container{}, metadata.Operation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.Container{}, metadata.Operation{}, metadata.ErrNotFound
	}
	container, ok := s.containers[containerID]
	if !ok {
		return metadata.Container{}, metadata.Operation{}, metadata.ErrNotFound
	}
	operation, ok := s.operations[operationID]
	if !ok {
		return metadata.Container{}, metadata.Operation{}, metadata.ErrNotFound
	}
	if container.State != containerFrom || operation.State != operationFrom ||
		!metadata.IsContainerTransitionValid(containerFrom, containerUpdate.State) ||
		!metadata.IsOperationTransitionValid(operationFrom, operationUpdate.State) {
		return metadata.Container{}, metadata.Operation{}, chamberErrors.ErrStateConflict
	}
	container = applyContainerUpdate(container, containerUpdate)
	operation = applyOperationUpdate(operation, operationUpdate)
	s.containers[containerID] = cloneContainer(container)
	s.operations[operationID] = cloneOperation(operation)
	return cloneContainer(container), cloneOperation(operation), nil
}

func (s *MemoryStore) FailContainerAndOperation(
	ctx context.Context,
	containerID string,
	from metadata.ContainerState,
	operationID string,
	code chamberErrors.Code,
) (metadata.Container, metadata.Operation, error) {
	now := time.Now().UTC()
	return s.TransitionContainerAndOperation(
		ctx,
		containerID,
		from,
		metadata.ContainerUpdate{
			OperationID:     operationID,
			State:           metadata.ContainerFailed,
			At:              now,
			ErrorCode:       code,
			ClearSupervisor: true,
		},
		operationID,
		metadata.OperationRunning,
		metadata.OperationUpdate{State: metadata.OperationFailed, At: now, ErrorCode: code},
	)
}

func (s *MemoryStore) CreateCleanup(ctx context.Context, cleanup metadata.Cleanup) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.ErrNotFound
	}
	if _, ok := s.cleanups[cleanup.ContainerID]; ok {
		return metadata.ErrAlreadyExists
	}
	s.cleanups[cleanup.ContainerID] = cleanup
	return nil
}

func (s *MemoryStore) ListCleanups(ctx context.Context) ([]metadata.Cleanup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, metadata.ErrNotFound
	}
	cleanups := make([]metadata.Cleanup, 0, len(s.cleanups))
	for _, cleanup := range s.cleanups {
		cleanups = append(cleanups, cleanup)
	}
	sort.Slice(cleanups, func(i, j int) bool {
		return cleanups[i].ContainerID < cleanups[j].ContainerID
	})
	return cleanups, nil
}

func (s *MemoryStore) ClaimCleanup(
	ctx context.Context,
	containerID string,
	leaseID string,
	at time.Time,
	expiresAt time.Time,
	reclaim bool,
) (metadata.Cleanup, error) {
	if err := ctx.Err(); err != nil {
		return metadata.Cleanup{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.Cleanup{}, metadata.ErrNotFound
	}
	cleanup, ok := s.cleanups[containerID]
	if !ok {
		return metadata.Cleanup{}, metadata.ErrNotFound
	}
	if !reclaim && cleanup.LeaseID != "" && cleanup.LeaseID != leaseID && cleanup.LeaseExpiresAt.After(at) {
		return metadata.Cleanup{}, metadata.ErrLeaseHeld
	}
	cleanup.LeaseID = leaseID
	cleanup.LeaseExpiresAt = expiresAt
	cleanup.UpdatedAt = at
	s.cleanups[containerID] = cleanup
	return cleanup, nil
}

func (s *MemoryStore) DeleteCleanup(ctx context.Context, containerID string, leaseID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return metadata.ErrNotFound
	}
	cleanup, ok := s.cleanups[containerID]
	if !ok {
		return metadata.ErrNotFound
	}
	if cleanup.LeaseID != leaseID {
		return metadata.ErrLeaseHeld
	}
	delete(s.cleanups, containerID)
	return nil
}

func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	return nil
}

func cloneOperation(operation metadata.Operation) metadata.Operation {
	operation.FinishedAt = cloneTimePtr(operation.FinishedAt)
	return operation
}

func applyOperationUpdate(operation metadata.Operation, update metadata.OperationUpdate) metadata.Operation {
	operation.State = update.State
	operation.UpdatedAt = update.At
	operation.FinishedAt = cloneTimePtr(&update.At)
	operation.ErrorCode = update.ErrorCode
	return operation
}

func applyContainerUpdate(container metadata.Container, update metadata.ContainerUpdate) metadata.Container {
	container.State = update.State
	if update.OperationID != "" {
		container.OperationID = update.OperationID
	}
	container.UpdatedAt = update.At
	container.ExitCode = cloneIntPtr(update.ExitCode)
	container.ErrorCode = update.ErrorCode
	if update.StdoutPath != "" {
		container.StdoutPath = update.StdoutPath
	}
	if update.StderrPath != "" {
		container.StderrPath = update.StderrPath
	}
	if update.RuntimeRoot != "" {
		container.RuntimeRoot = update.RuntimeRoot
	}
	if update.BundlePath != "" {
		container.BundlePath = update.BundlePath
	}
	if update.SupervisorPath != "" {
		container.SupervisorPath = update.SupervisorPath
	}
	if update.ClearSupervisor {
		container.SupervisorPID = 0
		container.SupervisorStartTime = 0
	} else if update.SupervisorPID != 0 {
		container.SupervisorPID = update.SupervisorPID
		container.SupervisorStartTime = update.SupervisorStartTime
	}
	return container
}

func cloneContainer(container metadata.Container) metadata.Container {
	container.ExitCode = cloneIntPtr(container.ExitCode)
	return container
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

var _ metadata.Store = (*MemoryStore)(nil)
