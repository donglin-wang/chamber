package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon/metadata"
	"github.com/donglin-wang/chamber/daemon/metadata/memory"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/google/uuid"
)

func TestHealth(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	newServer().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["status"] != "ok" {
		t.Fatalf("status body = %q, want ok", response["status"])
	}
}

func TestOpenAPIIsValidJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)

	newServer().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("openapi response is not valid JSON: %s", recorder.Body.String())
	}
}

func TestPullImageRequiresReference(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/pull", strings.NewReader(`{"reference":" "}`))

	mux := newServer()
	registerImageRoutes(mux, memory.NewMemoryStore(), fakeImageStore{})
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestPullImagePullsAndRecordsImage(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/pull", strings.NewReader(`{"reference":"docker.io/library/alpine:latest"}`))

	mux := newServer()
	registerImageRoutes(mux, memory.NewMemoryStore(), fakeImageStore{})
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response pullImageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	assertUUIDV7(t, response.OperationID)
	if response.Digest != "sha256:abc123" {
		t.Fatalf("digest = %q, want sha256:abc123", response.Digest)
	}
}

func TestPullImageRecordsPreciseSDKErrorCode(t *testing.T) {
	store := memory.NewMemoryStore()

	result, err := pullImage(
		context.Background(),
		store,
		fakeImageStore{err: fmt.Errorf("%w: bad ref", chamberErrors.ErrInvalidImageReference)},
		"not a reference",
	)
	if err == nil {
		t.Fatal("pullImage() error = nil, want SDK error")
	}
	if result.operation.ID == "" {
		t.Fatal("pullImage() operation ID = empty, want failed operation")
	}
	operation, getErr := store.GetOperation(context.Background(), result.operation.ID)
	if getErr != nil {
		t.Fatalf("GetOperation() error = %v", getErr)
	}
	if operation.ErrorCode != chamberErrors.ErrInvalidImageReference {
		t.Fatalf("operation ErrorCode = %q, want %q", operation.ErrorCode, chamberErrors.ErrInvalidImageReference)
	}
}

func TestRunContainerRequiresCommand(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/containers/run", strings.NewReader(`{"image":"docker.io/library/alpine:latest","command":[]}`))

	mux := newServer()
	registerContainerRoutes(mux, memory.NewMemoryStore(), nil, chamberRuntime.Config{}, nil, t.TempDir(), fakeStartSupervisor(nil), fakeOpenContainer(nil), nil)
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestRunContainerRecordsPreciseProvisionerErrorCode(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)

	result, err := runContainer(
		context.Background(),
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{err: fmt.Errorf("%w: bad mount", chamberErrors.ErrInvalidBundleMount)},
		t.TempDir(),
		fakeStartSupervisor(nil),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
		nil,
	)
	if err == nil {
		t.Fatal("runContainer() error = nil, want provisioner error")
	}
	if result.operation.ErrorCode != chamberErrors.ErrInvalidBundleMount {
		t.Fatalf("operation ErrorCode = %q, want %q", result.operation.ErrorCode, chamberErrors.ErrInvalidBundleMount)
	}
}

func TestRunContainerRecordsPreciseSupervisorErrorCode(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)

	result, err := runContainer(
		context.Background(),
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		t.TempDir(),
		fakeStartSupervisor(fmt.Errorf("%w: launch canceled", chamberErrors.ErrCanceled)),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
		nil,
	)
	if err == nil {
		t.Fatal("runContainer() error = nil, want runtime error")
	}
	if result.operation.ErrorCode != chamberErrors.ErrCanceled {
		t.Fatalf("operation ErrorCode = %q, want %q", result.operation.ErrorCode, chamberErrors.ErrCanceled)
	}
	if result.container.ErrorCode != chamberErrors.ErrCanceled {
		t.Fatalf("container ErrorCode = %q, want %q", result.container.ErrorCode, chamberErrors.ErrCanceled)
	}
}

func TestRunContainerStoresProvisionedBundlePath(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)

	provisionedBundlePath := "/tmp/chamber-test/provisioner-owned/container"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/containers/run", strings.NewReader(`{
		"image":"docker.io/library/alpine:latest",
		"command":["/bin/true"]
	}`))

	mux := newServer()
	registerContainerRoutes(
		mux,
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{bundlePath: provisionedBundlePath},
		t.TempDir(),
		fakeStartSupervisor(nil),
		fakeOpenContainer(nil),
		nil,
	)
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var response runContainerResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	container, err := store.GetContainer(context.Background(), response.ID)
	if err != nil {
		t.Fatalf("GetContainer(%q) error = %v", response.ID, err)
	}
	if container.BundlePath != provisionedBundlePath {
		t.Fatalf("BundlePath = %q, want provisioner-returned path %q", container.BundlePath, provisionedBundlePath)
	}
	if container.Runtime != "fake" {
		t.Fatalf("Runtime = %q, want fake runtime descriptor name", container.Runtime)
	}
}

func TestRunContainerRequestsNonTerminalProcess(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)
	var provisionRequest chamberBundle.ProvisionRequest

	_, err := runContainer(
		context.Background(),
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{
			bundlePath: "/tmp/chamber-test/provisioner-owned/container",
			request:    &provisionRequest,
		},
		t.TempDir(),
		fakeStartSupervisor(nil),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
		nil,
	)
	if err != nil {
		t.Fatalf("runContainer() error = %v", err)
	}
	if provisionRequest.Process.Terminal == nil {
		t.Fatal("Process.Terminal = nil, want explicit false")
	}
	if *provisionRequest.Process.Terminal {
		t.Fatal("Process.Terminal = true, want false for non-interactive daemon run")
	}
}

func TestRunContainerPassesMountsToProvisioner(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)
	var provisionRequest chamberBundle.ProvisionRequest

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/containers/run", strings.NewReader(`{
		"image":"docker.io/library/alpine:latest",
		"command":["/bin/true"],
		"mounts":[
			{"source":"/host/workspace","target":"/workspace","options":["rbind","ro"]}
		]
	}`))

	mux := newServer()
	registerContainerRoutes(
		mux,
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{
			bundlePath: "/tmp/chamber-test/provisioner-owned/container",
			request:    &provisionRequest,
		},
		t.TempDir(),
		fakeStartSupervisor(nil),
		fakeOpenContainer(nil),
		nil,
	)
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if len(provisionRequest.Mounts) != 1 {
		t.Fatalf("Mounts length = %d, want 1", len(provisionRequest.Mounts))
	}
	mount := provisionRequest.Mounts[0]
	if mount.Source != "/host/workspace" || mount.Target != "/workspace" {
		t.Fatalf("Mount = %#v, want workspace bind mount", mount)
	}
	if len(mount.Options) != 2 || mount.Options[0] != "rbind" || mount.Options[1] != "ro" {
		t.Fatalf("Mount.Options = %#v, want [rbind ro]", mount.Options)
	}
}

func TestRunContainerCanonicalizesImageBeforeLookup(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)

	_, err := runContainer(
		context.Background(),
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		t.TempDir(),
		fakeStartSupervisor(nil),
		"alpine",
		[]string{"/bin/true"},
		nil,
	)
	if err != nil {
		t.Fatalf("runContainer(short image) error = %v", err)
	}
}

func TestRunContainerPassesImagePlatformToProvisioner(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)
	var provisionRequest chamberBundle.ProvisionRequest

	_, err := runContainer(
		context.Background(),
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{
			bundlePath: "/tmp/chamber-test/provisioner-owned/container",
			request:    &provisionRequest,
		},
		t.TempDir(),
		fakeStartSupervisor(nil),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
		nil,
	)
	if err != nil {
		t.Fatalf("runContainer() error = %v", err)
	}
	if provisionRequest.ImagePlatform.Architecture != "arm64" {
		t.Fatalf("ImagePlatform = %#v, want persisted platform", provisionRequest.ImagePlatform)
	}
}

func TestGetContainerByIDReturnsState(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/containers/"+container.ID, nil)

	mux := newServer()
	registerContainerRoutes(mux, store, nil, chamberRuntime.Config{}, nil, t.TempDir(), fakeStartSupervisor(nil), fakeOpenContainer(nil), nil)
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response containerResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.ID != container.ID || response.State != metadata.ContainerRunning {
		t.Fatalf("response = %#v, want running container %q", response, container.ID)
	}
}

func TestCancelContainerForcesRuntimeAndBundleCleanup(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)
	var removed chamberBundle.ProvisionedBundle
	handle := &fakeContainerHandle{id: container.ID}
	var openedConfig chamberRuntime.Config
	var terminatedPID int

	canceled, err := cancelContainer(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake", RuntimeRoot: "/runtime/from-config"},
		fakeProvisioner{remove: &removed},
		func(ctx context.Context, config chamberRuntime.Config, containerID string) (chamberRuntime.ContainerHandle, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			openedConfig = config
			if containerID != container.ID {
				t.Fatalf("containerID = %q, want %q", containerID, container.ID)
			}
			return handle, nil
		},
		func(pid int) error {
			terminatedPID = pid
			return nil
		},
		container.ID,
	)
	if err != nil {
		t.Fatalf("cancelContainer() error = %v", err)
	}
	if !handle.deleted || !handle.deleteForce {
		t.Fatalf("runtime delete deleted=%v force=%v, want forced delete", handle.deleted, handle.deleteForce)
	}
	if removed.ContainerID != container.ID || removed.BundlePath != container.BundlePath {
		t.Fatalf("removed bundle = %#v, want container bundle", removed)
	}
	if openedConfig.RuntimeRoot != container.RuntimeRoot {
		t.Fatalf("runtime root = %q, want persisted container runtime root %q", openedConfig.RuntimeRoot, container.RuntimeRoot)
	}
	if terminatedPID != container.SupervisorPID {
		t.Fatalf("terminated supervisor PID = %d, want %d", terminatedPID, container.SupervisorPID)
	}
	if canceled.State != metadata.ContainerFailed || canceled.ErrorCode != chamberErrors.ErrCanceled {
		t.Fatalf("canceled container = %#v, want failed/canceled", canceled)
	}
	operation, err := store.GetOperation(context.Background(), container.OperationID)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if operation.State != metadata.OperationAborted || operation.ErrorCode != chamberErrors.ErrCanceled {
		t.Fatalf("operation = %#v, want aborted/canceled", operation)
	}
}

func TestRemoveContainerDeletesArtifactsAndMetadata(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerExited)
	var removed chamberBundle.ProvisionedBundle
	handle := &fakeContainerHandle{id: container.ID}
	supervisorDir := filepath.Dir(container.SupervisorPath)
	if err := os.MkdirAll(supervisorDir, 0700); err != nil {
		t.Fatalf("MkdirAll(supervisorDir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(supervisorDir, "stdout.log"), []byte("done\n"), 0600); err != nil {
		t.Fatalf("WriteFile(supervisor log) error = %v", err)
	}

	removedContainer, err := removeContainer(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{remove: &removed},
		func(ctx context.Context, _ chamberRuntime.Config, containerID string) (chamberRuntime.ContainerHandle, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if containerID != container.ID {
				t.Fatalf("containerID = %q, want %q", containerID, container.ID)
			}
			return handle, nil
		},
		nil,
		container.ID,
	)
	if err != nil {
		t.Fatalf("removeContainer() error = %v", err)
	}
	if removedContainer.ID != container.ID {
		t.Fatalf("removed container ID = %q, want %q", removedContainer.ID, container.ID)
	}
	if !handle.deleted || !handle.deleteForce || !handle.deletedStdout || !handle.deletedStderr {
		t.Fatalf("runtime cleanup handle = %#v, want forced delete and log deletion", handle)
	}
	if removed.ContainerID != container.ID || removed.BundlePath != container.BundlePath {
		t.Fatalf("removed bundle = %#v, want container bundle", removed)
	}
	if _, err := os.Stat(supervisorDir); !os.IsNotExist(err) {
		t.Fatalf("supervisor dir stat error = %v, want not exist", err)
	}
	if _, err := store.GetContainer(context.Background(), container.ID); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatalf("GetContainer(removed) error = %v, want %v", err, metadata.ErrNotFound)
	}
}

func createTestContainer(t *testing.T, store metadata.Store, state metadata.ContainerState) metadata.Container {
	t.Helper()

	now := time.Now().UTC()
	operation := metadata.Operation{
		ID:         "operation-" + string(state),
		Kind:       metadata.RunOperation,
		State:      metadata.OperationRunning,
		ResourceID: "container-" + string(state),
		StartedAt:  now,
		UpdatedAt:  now,
	}
	if err := store.CreateOperation(context.Background(), operation); err != nil {
		t.Fatalf("CreateOperation() error = %v", err)
	}
	container := metadata.Container{
		ID:             operation.ResourceID,
		OperationID:    operation.ID,
		ImageRef:       "docker.io/library/alpine:latest",
		ImageDigest:    "sha256:image",
		BundlePath:     "/tmp/chamber-test/bundles/" + operation.ResourceID,
		Runtime:        "fake",
		RuntimeRoot:    "/tmp/chamber-test/runtime",
		SupervisorPath: "/tmp/chamber-test/supervisors/" + operation.ResourceID + "/supervisor.json",
		SupervisorPID:  1234,
		State:          state,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := store.CreateContainer(context.Background(), container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}
	return container
}

func putTestImage(t *testing.T, store metadata.Store) {
	t.Helper()

	if err := store.PutImage(context.Background(), metadata.Image{
		Reference: "index.docker.io/library/alpine:latest",
		Digest:    "sha256:image",
		Platform: chamberImage.Platform{
			OS:           "linux",
			Architecture: "arm64",
		},
		PulledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutImage() error = %v", err)
	}
}

func TestContainerLogsReadByContainerID(t *testing.T) {
	store := memory.NewMemoryStore()
	container := metadata.Container{
		ID:          "container-1",
		OperationID: "operation-1",
		ImageRef:    "docker.io/library/alpine:latest",
		ImageDigest: "sha256:image",
		BundlePath:  "/tmp/chamber-test/not-a-log-location",
		Runtime:     "runc",
		State:       metadata.ContainerExited,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	logDir := t.TempDir()
	stderrPath := filepath.Join(logDir, "stderr.log")
	if err := os.WriteFile(stderrPath, []byte("hello stderr"), 0600); err != nil {
		t.Fatalf("WriteFile(stderr log) error = %v", err)
	}
	container.StderrPath = stderrPath
	if err := store.CreateContainer(context.Background(), container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/containers/container-1/logs?stream=stderr", nil)

	mux := newServer()
	registerContainerRoutes(mux, store, nil, chamberRuntime.Config{}, nil, t.TempDir(), fakeStartSupervisor(nil), fakeOpenContainer(nil), nil)
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if recorder.Body.String() != "hello stderr" {
		t.Fatalf("log body = %q, want hello stderr", recorder.Body.String())
	}
}

type fakeProvisioner struct {
	bundlePath string
	err        error
	request    *chamberBundle.ProvisionRequest
	remove     *chamberBundle.ProvisionedBundle
}

func (p fakeProvisioner) Descriptor() chamberBundle.Descriptor {
	return chamberBundle.Descriptor{Name: "fake"}
}

func (p fakeProvisioner) Provision(ctx context.Context, request chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
	if err := ctx.Err(); err != nil {
		return chamberBundle.ProvisionedBundle{}, err
	}
	if p.err != nil {
		return chamberBundle.ProvisionedBundle{}, p.err
	}
	if p.request != nil {
		*p.request = request
	}
	return chamberBundle.ProvisionedBundle{
		ContainerID: request.ContainerID,
		BundlePath:  p.bundlePath,
	}, nil
}

func (p fakeProvisioner) Remove(ctx context.Context, bundle chamberBundle.ProvisionedBundle) error {
	if p.remove != nil {
		*p.remove = bundle
	}
	return ctx.Err()
}

func fakeStartSupervisor(startErr error) startSupervisorFunc {
	return func(ctx context.Context, _ string) (int, func() error, error) {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		if startErr != nil {
			return 0, nil, startErr
		}
		return 1234, nil, nil
	}
}

func fakeOpenContainer(openErr error) openContainerFunc {
	return func(ctx context.Context, _ chamberRuntime.Config, containerID string) (chamberRuntime.ContainerHandle, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if openErr != nil {
			return nil, openErr
		}
		return &fakeContainerHandle{id: containerID}, nil
	}
}

type fakeContainerHandle struct {
	id             string
	deleted        bool
	deleteForce    bool
	deleteErr      error
	deletedStdout  bool
	deletedStderr  bool
	deleteLogError error
}

func (h *fakeContainerHandle) ID() string {
	return h.id
}

func (h *fakeContainerHandle) StdoutPath() string {
	return ""
}

func (h *fakeContainerHandle) StderrPath() string {
	return ""
}

func (h *fakeContainerHandle) State(context.Context) (chamberRuntime.ContainerState, error) {
	return chamberRuntime.ContainerState{ContainerID: h.id, Status: chamberRuntime.ContainerStatusRunning}, nil
}

func (h *fakeContainerHandle) Signal(context.Context, os.Signal) error {
	return nil
}

func (h *fakeContainerHandle) Delete(_ context.Context, force bool) error {
	h.deleted = true
	h.deleteForce = force
	return h.deleteErr
}

func (h *fakeContainerHandle) ReadLog(chamberRuntime.LogStream) ([]byte, error) {
	return nil, nil
}

func (h *fakeContainerHandle) DeleteLog(stream chamberRuntime.LogStream) error {
	if h.deleteLogError != nil {
		return h.deleteLogError
	}
	switch stream {
	case chamberRuntime.StdoutLogStream:
		h.deletedStdout = true
	case chamberRuntime.StderrLogStream:
		h.deletedStderr = true
	}
	return nil
}

type fakeImageStore struct {
	err        error
	layoutPath string
}

func (s fakeImageStore) Pull(ctx context.Context, request chamberImage.PullRequest) (chamberImage.Image, error) {
	if err := ctx.Err(); err != nil {
		return chamberImage.Image{}, err
	}
	if s.err != nil {
		return chamberImage.Image{}, s.err
	}
	return chamberImage.Image{
		Reference: request.Reference,
		Digest:    "sha256:abc123",
		Platform:  chamberImage.NormalizePlatform(request.Platform),
		Source:    chamberImage.SourcePulled,
		CreatedAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (s fakeImageStore) Build(ctx context.Context, request chamberImage.BuildRequest) (chamberImage.Image, error) {
	return chamberImage.Image{}, chamberErrors.ErrInvalidRequest
}

func (s fakeImageStore) List(ctx context.Context, request chamberImage.ListRequest) ([]chamberImage.Image, error) {
	return nil, nil
}

func (s fakeImageStore) Remove(ctx context.Context, request chamberImage.RemoveRequest) error {
	return nil
}

func (s fakeImageStore) Layout(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.layoutPath != "" {
		return s.layoutPath, nil
	}
	return "/tmp/chamber-test/images/layout", nil
}

func assertUUIDV7(t *testing.T, raw string) {
	t.Helper()

	id, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", raw, err)
	}
	if id[6]>>4 != 7 {
		t.Fatalf("uuid version = %d, want 7", id[6]>>4)
	}
}
