package memory

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
)

type MemoryStore struct {
	mu         sync.RWMutex
	images     map[string]metadata.Image
	operations map[string]metadata.Operation
	containers map[string]metadata.Container
	closed     bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		images:     make(map[string]metadata.Image),
		operations: make(map[string]metadata.Operation),
		containers: make(map[string]metadata.Container),
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

func (s *MemoryStore) SucceedOperation(ctx context.Context, id string) (metadata.Operation, error) {
	return s.TransitionOperation(ctx, id, metadata.OperationRunning, metadata.OperationUpdate{
		State: metadata.OperationSucceeded,
		At:    time.Now().UTC(),
	})
}

func (s *MemoryStore) FailOperation(ctx context.Context, id string, code metadata.ErrorCode) (metadata.Operation, error) {
	return s.TransitionOperation(ctx, id, metadata.OperationRunning, metadata.OperationUpdate{
		State:     metadata.OperationFailed,
		At:        time.Now().UTC(),
		ErrorCode: string(code),
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
		return metadata.Operation{}, metadata.ErrStateConflict
	}
	if !metadata.IsOperationTransitionValid(from, update.State) {
		return metadata.Operation{}, metadata.ErrStateConflict
	}

	operation.State = update.State
	operation.UpdatedAt = update.At
	operation.FinishedAt = cloneTimePtr(&update.At)
	operation.ErrorCode = metadata.ErrorCode(update.ErrorCode)
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
		return metadata.Container{}, metadata.ErrStateConflict
	}
	if !metadata.IsContainerTransitionValid(from, update.State) {
		return metadata.Container{}, metadata.ErrStateConflict
	}

	container.State = update.State
	container.UpdatedAt = update.At
	container.ExitCode = cloneIntPtr(update.ExitCode)
	container.ErrorCode = metadata.ErrorCode(update.ErrorCode)
	s.containers[id] = cloneContainer(container)
	return cloneContainer(container), nil
}

func (s *MemoryStore) FailContainerAndOperation(
	ctx context.Context,
	containerID string,
	from metadata.ContainerState,
	operationID string,
	code metadata.ErrorCode,
) (metadata.Container, metadata.Operation, error) {
	container, containerErr := s.TransitionContainer(ctx, containerID, from, metadata.ContainerUpdate{
		State:     metadata.ContainerFailed,
		At:        time.Now().UTC(),
		ErrorCode: string(code),
	})
	operation, operationErr := s.FailOperation(ctx, operationID, code)
	return container, operation, errors.Join(containerErr, operationErr)
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
