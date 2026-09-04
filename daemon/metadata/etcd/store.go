package etcd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

const (
	schemaVersion = 1

	imagePrefix     = "/chamber/v0/images/by-reference/"
	operationPrefix = "/chamber/v0/operations/"
	containerPrefix = "/chamber/v0/containers/"
	cleanupPrefix   = "/chamber/v0/cleanups/"
)

type Store struct {
	client *clientv3.Client
	server *embed.Etcd

	closeOnce sync.Once
	closeErr  error
}

type envelope[T any] struct {
	SchemaVersion int `json:"schema_version"`
	Value         T   `json:"value"`
}

func Open(ctx context.Context, cfg metadata.Config, workspace *hostfs.Workspace) (*Store, error) {
	if workspace == nil {
		return nil, fmt.Errorf("metadata etcd: workspace is required")
	}
	if cfg.Root == "" {
		return nil, fmt.Errorf("metadata etcd: root is required")
	}

	dataDir, err := absPath(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: resolve root: %w", err)
	}
	if filepath.Clean(dataDir) != filepath.Clean(workspace.Root()) {
		return nil, fmt.Errorf("metadata etcd: root %q does not match workspace root %q", cfg.Root, workspace.Root())
	}
	if err := requireWorkspaceFeatures("metadata workspace", workspace.Features(), hostfs.FeatureSet{
		PrivateDirs:      true,
		FileFsync:        true,
		AtomicFileRename: true,
		DirectoryFsync:   true,
	}); err != nil {
		return nil, err
	}
	dataDir = workspace.Root()

	clientSocket := filepath.Join(dataDir, "client.sock")
	peerSocket := filepath.Join(dataDir, "peer.sock")
	if _, err := workspace.MkdirPrivate("."); err != nil {
		return nil, fmt.Errorf("metadata etcd: create client socket dir: %w", err)
	}
	if _, err := workspace.MkdirPrivate("."); err != nil {
		return nil, fmt.Errorf("metadata etcd: create peer socket dir: %w", err)
	}

	clientURL, err := unixURL(clientSocket)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: client socket URL: %w", err)
	}
	listenPeerURL, err := unixPeerListenURL(peerSocket)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: peer socket URL: %w", err)
	}
	advertisePeerURL, err := unixURL(peerSocket)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: peer advertise URL: %w", err)
	}

	embedConfig := embed.NewConfig()
	embedConfig.Dir = dataDir
	embedConfig.Name = "chamber"
	embedConfig.ListenClientUrls = []url.URL{clientURL}
	embedConfig.AdvertiseClientUrls = []url.URL{clientURL}
	embedConfig.ListenPeerUrls = []url.URL{listenPeerURL}
	embedConfig.AdvertisePeerUrls = []url.URL{advertisePeerURL}
	embedConfig.InitialCluster = fmt.Sprintf("%s=%s", embedConfig.Name, advertisePeerURL.String())
	embedConfig.LogLevel = "error"

	server, err := embed.StartEtcd(embedConfig)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: start embedded server: %w", err)
	}

	select {
	case <-server.Server.ReadyNotify():
	case err := <-server.Err():
		server.Close()
		return nil, fmt.Errorf("metadata etcd: server stopped before ready: %w", err)
	case <-ctx.Done():
		server.Close()
		return nil, ctx.Err()
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL.String()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		server.Close()
		return nil, fmt.Errorf("metadata etcd: create client: %w", err)
	}

	return &Store{
		client: client,
		server: server,
	}, nil
}

func requireWorkspaceFeatures(label string, observed hostfs.FeatureSet, required hostfs.FeatureSet) error {
	if required.PrivateDirs && !observed.PrivateDirs {
		return fmt.Errorf("metadata etcd: %s requires private directories", label)
	}
	if required.FileFsync && !observed.FileFsync {
		return fmt.Errorf("metadata etcd: %s requires file fsync", label)
	}
	if required.DirectoryFsync && !observed.DirectoryFsync {
		return fmt.Errorf("metadata etcd: %s requires directory fsync", label)
	}
	if required.AtomicFileRename && !observed.AtomicFileRename {
		return fmt.Errorf("metadata etcd: %s requires atomic file rename between temporary and durable roots", label)
	}
	if required.AtomicDirectoryRename && !observed.AtomicDirectoryRename {
		return fmt.Errorf("metadata etcd: %s requires atomic directory rename between temporary and durable roots", label)
	}
	return nil
}

func (s *Store) PutImage(ctx context.Context, image metadata.Image) error {
	payload, err := marshalValue(image)
	if err != nil {
		return err
	}
	_, err = s.client.Put(ctx, imageKey(image.Reference), payload)
	return mapEtcdError(err)
}

func (s *Store) GetImage(ctx context.Context, reference string) (metadata.Image, error) {
	return getValue[metadata.Image](ctx, s.client, imageKey(reference))
}

func (s *Store) CreateOperation(ctx context.Context, operation metadata.Operation) error {
	return createValue(ctx, s.client, operationKey(operation.ID), operation)
}

func (s *Store) GetOperation(ctx context.Context, id string) (metadata.Operation, error) {
	return getValue[metadata.Operation](ctx, s.client, operationKey(id))
}

func (s *Store) ListOperations(ctx context.Context) ([]metadata.Operation, error) {
	response, err := s.client.Get(ctx, operationPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, mapEtcdError(err)
	}
	operations := make([]metadata.Operation, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		operation, err := unmarshalValue[metadata.Operation](kv.Value)
		if err != nil {
			return nil, err
		}
		operations = append(operations, cloneOperation(operation))
	}
	sort.Slice(operations, func(i, j int) bool {
		return operations[i].ID < operations[j].ID
	})
	return operations, nil
}

func (s *Store) SucceedOperation(ctx context.Context, id string) (metadata.Operation, error) {
	return s.TransitionOperation(ctx, id, metadata.OperationRunning, metadata.OperationUpdate{
		State: metadata.OperationSucceeded,
		At:    time.Now().UTC(),
	})
}

func (s *Store) FailOperation(ctx context.Context, id string, code chamberErrors.Code) (metadata.Operation, error) {
	return s.TransitionOperation(ctx, id, metadata.OperationRunning, metadata.OperationUpdate{
		State:     metadata.OperationFailed,
		At:        time.Now().UTC(),
		ErrorCode: code,
	})
}

func (s *Store) TransitionOperation(
	ctx context.Context,
	id string,
	from metadata.OperationState,
	update metadata.OperationUpdate,
) (metadata.Operation, error) {
	key := operationKey(id)
	operation, modRevision, err := getValueWithRevision[metadata.Operation](ctx, s.client, key)
	if err != nil {
		return metadata.Operation{}, err
	}
	if operation.State != from {
		return metadata.Operation{}, chamberErrors.ErrStateConflict
	}
	if !metadata.IsOperationTransitionValid(from, update.State) {
		return metadata.Operation{}, chamberErrors.ErrStateConflict
	}

	operation = applyOperationUpdate(operation, update)

	if err := compareAndPut(ctx, s.client, key, modRevision, operation); err != nil {
		return metadata.Operation{}, err
	}
	return cloneOperation(operation), nil
}

func (s *Store) CreateContainer(ctx context.Context, container metadata.Container) error {
	return createValue(ctx, s.client, containerKey(container.ID), container)
}

func (s *Store) CreateContainerAndOperation(ctx context.Context, container metadata.Container, operation metadata.Operation) error {
	containerPayload, err := marshalValue(container)
	if err != nil {
		return err
	}
	operationPayload, err := marshalValue(operation)
	if err != nil {
		return err
	}
	containerKey := containerKey(container.ID)
	operationKey := operationKey(operation.ID)
	response, err := s.client.Txn(ctx).
		If(
			clientv3.Compare(clientv3.CreateRevision(containerKey), "=", 0),
			clientv3.Compare(clientv3.CreateRevision(operationKey), "=", 0),
		).
		Then(
			clientv3.OpPut(containerKey, containerPayload),
			clientv3.OpPut(operationKey, operationPayload),
		).
		Commit()
	if err != nil {
		return mapEtcdError(err)
	}
	if !response.Succeeded {
		return metadata.ErrAlreadyExists
	}
	return nil
}

func (s *Store) CreateOperationAndTransitionContainer(
	ctx context.Context,
	operation metadata.Operation,
	containerID string,
	containerFrom metadata.ContainerState,
	containerUpdate metadata.ContainerUpdate,
) (metadata.Container, error) {
	containerStorageKey := containerKey(containerID)
	container, containerRevision, err := getValueWithRevision[metadata.Container](ctx, s.client, containerStorageKey)
	if err != nil {
		return metadata.Container{}, err
	}
	if container.State != containerFrom || !metadata.IsContainerTransitionValid(containerFrom, containerUpdate.State) {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	container = applyContainerUpdate(container, containerUpdate)
	containerPayload, err := marshalValue(container)
	if err != nil {
		return metadata.Container{}, err
	}
	operationPayload, err := marshalValue(operation)
	if err != nil {
		return metadata.Container{}, err
	}
	operationStorageKey := operationKey(operation.ID)
	response, err := s.client.Txn(ctx).
		If(
			clientv3.Compare(clientv3.ModRevision(containerStorageKey), "=", containerRevision),
			clientv3.Compare(clientv3.CreateRevision(operationStorageKey), "=", 0),
		).
		Then(
			clientv3.OpPut(containerStorageKey, containerPayload),
			clientv3.OpPut(operationStorageKey, operationPayload),
		).
		Commit()
	if err != nil {
		return metadata.Container{}, mapEtcdError(err)
	}
	if !response.Succeeded {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	return cloneContainer(container), nil
}

func (s *Store) GetContainer(ctx context.Context, id string) (metadata.Container, error) {
	return getValue[metadata.Container](ctx, s.client, containerKey(id))
}

func (s *Store) ListContainers(ctx context.Context) ([]metadata.Container, error) {
	response, err := s.client.Get(ctx, containerPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, mapEtcdError(err)
	}

	containers := make([]metadata.Container, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		container, err := unmarshalValue[metadata.Container](kv.Value)
		if err != nil {
			return nil, err
		}
		containers = append(containers, cloneContainer(container))
	}
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].ID < containers[j].ID
	})
	return containers, nil
}

func (s *Store) DeleteContainer(ctx context.Context, id string) (metadata.Container, error) {
	key := containerKey(id)
	container, modRevision, err := getValueWithRevision[metadata.Container](ctx, s.client, key)
	if err != nil {
		return metadata.Container{}, err
	}
	response, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", modRevision)).
		Then(clientv3.OpDelete(key)).
		Commit()
	if err != nil {
		return metadata.Container{}, mapEtcdError(err)
	}
	if !response.Succeeded {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	return cloneContainer(container), nil
}

func (s *Store) TransitionContainer(
	ctx context.Context,
	id string,
	from metadata.ContainerState,
	update metadata.ContainerUpdate,
) (metadata.Container, error) {
	key := containerKey(id)
	container, modRevision, err := getValueWithRevision[metadata.Container](ctx, s.client, key)
	if err != nil {
		return metadata.Container{}, err
	}
	if container.State != from {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	if !metadata.IsContainerTransitionValid(from, update.State) {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}

	container = applyContainerUpdate(container, update)

	if err := compareAndPut(ctx, s.client, key, modRevision, container); err != nil {
		return metadata.Container{}, err
	}
	return cloneContainer(container), nil
}

func (s *Store) SetContainerSupervisor(ctx context.Context, id string, state metadata.ContainerState, pid int, startTime uint64, at time.Time) (metadata.Container, error) {
	key := containerKey(id)
	container, modRevision, err := getValueWithRevision[metadata.Container](ctx, s.client, key)
	if err != nil {
		return metadata.Container{}, err
	}
	if container.State != state {
		return metadata.Container{}, chamberErrors.ErrStateConflict
	}
	container.SupervisorPID = pid
	container.SupervisorStartTime = startTime
	container.UpdatedAt = at
	if err := compareAndPut(ctx, s.client, key, modRevision, container); err != nil {
		return metadata.Container{}, err
	}
	return cloneContainer(container), nil
}

func (s *Store) TransitionContainerAndOperation(
	ctx context.Context,
	containerID string,
	containerFrom metadata.ContainerState,
	containerUpdate metadata.ContainerUpdate,
	operationID string,
	operationFrom metadata.OperationState,
	operationUpdate metadata.OperationUpdate,
) (metadata.Container, metadata.Operation, error) {
	containerStorageKey := containerKey(containerID)
	container, containerRevision, err := getValueWithRevision[metadata.Container](ctx, s.client, containerStorageKey)
	if err != nil {
		return metadata.Container{}, metadata.Operation{}, err
	}
	operationStorageKey := operationKey(operationID)
	operation, operationRevision, err := getValueWithRevision[metadata.Operation](ctx, s.client, operationStorageKey)
	if err != nil {
		return metadata.Container{}, metadata.Operation{}, err
	}
	if container.State != containerFrom || operation.State != operationFrom ||
		!metadata.IsContainerTransitionValid(containerFrom, containerUpdate.State) ||
		!metadata.IsOperationTransitionValid(operationFrom, operationUpdate.State) {
		return metadata.Container{}, metadata.Operation{}, chamberErrors.ErrStateConflict
	}
	container = applyContainerUpdate(container, containerUpdate)
	operation = applyOperationUpdate(operation, operationUpdate)
	containerPayload, err := marshalValue(container)
	if err != nil {
		return metadata.Container{}, metadata.Operation{}, err
	}
	operationPayload, err := marshalValue(operation)
	if err != nil {
		return metadata.Container{}, metadata.Operation{}, err
	}
	response, err := s.client.Txn(ctx).
		If(
			clientv3.Compare(clientv3.ModRevision(containerStorageKey), "=", containerRevision),
			clientv3.Compare(clientv3.ModRevision(operationStorageKey), "=", operationRevision),
		).
		Then(
			clientv3.OpPut(containerStorageKey, containerPayload),
			clientv3.OpPut(operationStorageKey, operationPayload),
		).
		Commit()
	if err != nil {
		return metadata.Container{}, metadata.Operation{}, mapEtcdError(err)
	}
	if !response.Succeeded {
		return metadata.Container{}, metadata.Operation{}, chamberErrors.ErrStateConflict
	}
	return cloneContainer(container), cloneOperation(operation), nil
}

func (s *Store) FailContainerAndOperation(
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

func (s *Store) CreateCleanup(ctx context.Context, cleanup metadata.Cleanup) error {
	return createValue(ctx, s.client, cleanupKey(cleanup.ContainerID), cleanup)
}

func (s *Store) ListCleanups(ctx context.Context) ([]metadata.Cleanup, error) {
	response, err := s.client.Get(ctx, cleanupPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, mapEtcdError(err)
	}
	cleanups := make([]metadata.Cleanup, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		cleanup, err := unmarshalValue[metadata.Cleanup](kv.Value)
		if err != nil {
			return nil, err
		}
		cleanups = append(cleanups, cleanup)
	}
	sort.Slice(cleanups, func(i, j int) bool {
		return cleanups[i].ContainerID < cleanups[j].ContainerID
	})
	return cleanups, nil
}

func (s *Store) ClaimCleanup(
	ctx context.Context,
	containerID string,
	leaseID string,
	at time.Time,
	expiresAt time.Time,
	reclaim bool,
) (metadata.Cleanup, error) {
	key := cleanupKey(containerID)
	cleanup, modRevision, err := getValueWithRevision[metadata.Cleanup](ctx, s.client, key)
	if err != nil {
		return metadata.Cleanup{}, err
	}
	if !reclaim && cleanup.LeaseID != "" && cleanup.LeaseID != leaseID && cleanup.LeaseExpiresAt.After(at) {
		return metadata.Cleanup{}, metadata.ErrLeaseHeld
	}
	cleanup.LeaseID = leaseID
	cleanup.LeaseExpiresAt = expiresAt
	cleanup.UpdatedAt = at
	if err := compareAndPut(ctx, s.client, key, modRevision, cleanup); err != nil {
		return metadata.Cleanup{}, err
	}
	return cleanup, nil
}

func (s *Store) DeleteCleanup(ctx context.Context, containerID string, leaseID string) error {
	key := cleanupKey(containerID)
	cleanup, modRevision, err := getValueWithRevision[metadata.Cleanup](ctx, s.client, key)
	if err != nil {
		return err
	}
	if cleanup.LeaseID != leaseID {
		return metadata.ErrLeaseHeld
	}
	response, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", modRevision)).
		Then(clientv3.OpDelete(key)).
		Commit()
	if err != nil {
		return mapEtcdError(err)
	}
	if !response.Succeeded {
		return chamberErrors.ErrStateConflict
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.client != nil {
			s.closeErr = s.client.Close()
		}
		if s.server != nil {
			s.server.Close()
		}
	})
	return s.closeErr
}

func createValue[T any](ctx context.Context, client *clientv3.Client, key string, value T) error {
	payload, err := marshalValue(value)
	if err != nil {
		return err
	}

	response, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, payload)).
		Commit()
	if err != nil {
		return mapEtcdError(err)
	}
	if !response.Succeeded {
		return metadata.ErrAlreadyExists
	}
	return nil
}

func getValue[T any](ctx context.Context, client *clientv3.Client, key string) (T, error) {
	value, _, err := getValueWithRevision[T](ctx, client, key)
	return value, err
}

func getValueWithRevision[T any](ctx context.Context, client *clientv3.Client, key string) (T, int64, error) {
	var zero T

	response, err := client.Get(ctx, key)
	if err != nil {
		return zero, 0, mapEtcdError(err)
	}
	if len(response.Kvs) == 0 {
		return zero, 0, metadata.ErrNotFound
	}
	if len(response.Kvs) > 1 {
		return zero, 0, metadataFailure("expected one value for key %q, got %d", key, len(response.Kvs))
	}

	value, err := unmarshalValue[T](response.Kvs[0].Value)
	if err != nil {
		return zero, 0, err
	}
	return value, response.Kvs[0].ModRevision, nil
}

func compareAndPut[T any](ctx context.Context, client *clientv3.Client, key string, modRevision int64, value T) error {
	payload, err := marshalValue(value)
	if err != nil {
		return err
	}

	response, err := client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", modRevision)).
		Then(clientv3.OpPut(key, payload)).
		Commit()
	if err != nil {
		return mapEtcdError(err)
	}
	if !response.Succeeded {
		return chamberErrors.ErrStateConflict
	}
	return nil
}

func marshalValue[T any](value T) (string, error) {
	payload, err := json.Marshal(envelope[T]{
		SchemaVersion: schemaVersion,
		Value:         value,
	})
	if err != nil {
		return "", metadataFailure("encode value: %v", err)
	}
	return string(payload), nil
}

func unmarshalValue[T any](payload []byte) (T, error) {
	var zero T
	var wrapped envelope[T]
	if err := json.Unmarshal(payload, &wrapped); err != nil {
		return zero, metadataFailure("decode value: %v", err)
	}
	if wrapped.SchemaVersion != schemaVersion {
		return zero, metadataFailure("unsupported schema version %d", wrapped.SchemaVersion)
	}
	return wrapped.Value, nil
}

func imageKey(reference string) string {
	escaped := base64.RawURLEncoding.EncodeToString([]byte(reference))
	return imagePrefix + escaped
}

func operationKey(id string) string {
	return operationPrefix + id
}

func containerKey(id string) string {
	return containerPrefix + id
}

func cleanupKey(containerID string) string {
	return cleanupPrefix + containerID
}

func unixURL(socketPath string) (url.URL, error) {
	absolutePath, err := filepath.Abs(socketPath)
	if err != nil {
		return url.URL{}, err
	}
	parsed, err := url.Parse("unix://" + absolutePath)
	if err != nil {
		return url.URL{}, err
	}
	return *parsed, nil
}

func unixPeerListenURL(socketPath string) (url.URL, error) {
	absolutePath, err := filepath.Abs(socketPath)
	if err != nil {
		return url.URL{}, err
	}
	return url.URL{Scheme: "unix", Host: absolutePath}, nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

func mapEtcdError(err error) error {
	if err == nil {
		return nil
	}
	return metadataFailure("%v", err)
}

func metadataFailure(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{chamberErrors.ErrMetadataFailed}, args...)...)
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

var _ metadata.Store = (*Store)(nil)
