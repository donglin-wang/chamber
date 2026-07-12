package etcd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/donglin-wang/chamber/internal/fsutil"
	"github.com/donglin-wang/chamber/internal/metadata"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

const (
	schemaVersion = 1

	imagePrefix     = "/chamber/v0/images/by-reference/"
	operationPrefix = "/chamber/v0/operations/"
	containerPrefix = "/chamber/v0/containers/"
)

type Config struct {
	DataDir      string
	ClientSocket string
	PeerSocket   string
}

type Override struct {
	DataDir      *string
	ClientSocket *string
	PeerSocket   *string
}

func DefaultConfig(rootPath string) Config {
	return Config{
		DataDir: filepath.Join(rootPath, "metadata", "etcd"),
	}
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.DataDir != nil {
		defaultConfig.DataDir = *override.DataDir
	}
	if override.ClientSocket != nil {
		defaultConfig.ClientSocket = *override.ClientSocket
	}
	if override.PeerSocket != nil {
		defaultConfig.PeerSocket = *override.PeerSocket
	}

	var err error
	defaultConfig.DataDir, err = absPath(defaultConfig.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("metadata etcd: resolve data dir: %w", err)
	}
	defaultConfig.ClientSocket, err = absPath(defaultConfig.ClientSocket)
	if err != nil {
		return Config{}, fmt.Errorf("metadata etcd: resolve client socket: %w", err)
	}
	defaultConfig.PeerSocket, err = absPath(defaultConfig.PeerSocket)
	if err != nil {
		return Config{}, fmt.Errorf("metadata etcd: resolve peer socket: %w", err)
	}

	return defaultConfig, nil
}

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

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("metadata etcd: data dir is required")
	}

	dataDir, err := absPath(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: resolve data dir: %w", err)
	}
	if err := fsutil.EnsurePrivateDir(dataDir); err != nil {
		return nil, fmt.Errorf("metadata etcd: prepare data dir: %w", err)
	}

	if cfg.ClientSocket == "" {
		cfg.ClientSocket = filepath.Join(dataDir, "client.sock")
	}
	if cfg.PeerSocket == "" {
		cfg.PeerSocket = filepath.Join(dataDir, "peer.sock")
	}
	if err := fsutil.EnsurePrivateParent(cfg.ClientSocket); err != nil {
		return nil, fmt.Errorf("metadata etcd: prepare client socket dir: %w", err)
	}
	if err := fsutil.EnsurePrivateParent(cfg.PeerSocket); err != nil {
		return nil, fmt.Errorf("metadata etcd: prepare peer socket dir: %w", err)
	}

	clientURL, err := unixURL(cfg.ClientSocket)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: client socket URL: %w", err)
	}
	listenPeerURL, err := unixPeerListenURL(cfg.PeerSocket)
	if err != nil {
		return nil, fmt.Errorf("metadata etcd: peer socket URL: %w", err)
	}
	advertisePeerURL, err := unixURL(cfg.PeerSocket)
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
		return metadata.Operation{}, metadata.ErrStateConflict
	}
	if !metadata.IsOperationTransitionValid(from, update.State) {
		return metadata.Operation{}, metadata.ErrStateConflict
	}

	operation.State = update.State
	operation.UpdatedAt = update.At
	operation.FinishedAt = cloneTimePtr(&update.At)
	operation.ErrorCode = metadata.ErrorCode(update.ErrorCode)

	if err := compareAndPut(ctx, s.client, key, modRevision, operation); err != nil {
		return metadata.Operation{}, err
	}
	return cloneOperation(operation), nil
}

func (s *Store) CreateContainer(ctx context.Context, container metadata.Container) error {
	return createValue(ctx, s.client, containerKey(container.ID), container)
}

func (s *Store) GetContainer(ctx context.Context, id string) (metadata.Container, error) {
	return getValue[metadata.Container](ctx, s.client, containerKey(id))
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
		return metadata.Container{}, metadata.ErrStateConflict
	}
	if !metadata.IsContainerTransitionValid(from, update.State) {
		return metadata.Container{}, metadata.ErrStateConflict
	}

	container.State = update.State
	container.UpdatedAt = update.At
	container.ExitCode = cloneIntPtr(update.ExitCode)
	container.ErrorCode = metadata.ErrorCode(update.ErrorCode)

	if err := compareAndPut(ctx, s.client, key, modRevision, container); err != nil {
		return metadata.Container{}, err
	}
	return cloneContainer(container), nil
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
		return metadata.ErrStateConflict
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
	return fmt.Errorf("%w: "+format, append([]any{metadata.ErrMetadataFailed}, args...)...)
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

var _ metadata.Store = (*Store)(nil)
