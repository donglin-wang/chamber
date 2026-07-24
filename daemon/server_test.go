package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chamberDaemonConfig "github.com/donglin-wang/chamber/daemon/config"
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
	registerImageRoutes(mux, testConfig(t), memory.NewMemoryStore(), fakePuller{})
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestPullImagePullsAndRecordsImage(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/pull", strings.NewReader(`{"reference":"docker.io/library/alpine:latest"}`))

	mux := newServer()
	registerImageRoutes(mux, testConfig(t), memory.NewMemoryStore(), fakePuller{})
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
		fakePuller{err: fmt.Errorf("%w: bad ref", chamberErrors.ErrInvalidImageReference)},
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
	registerContainerRoutes(mux, memory.NewMemoryStore(), nil, nil, context.Background())
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
		fakeRuntime{},
		fakeProvisioner{err: fmt.Errorf("%w: bad mount", chamberErrors.ErrInvalidBundleMount)},
		context.Background(),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
	)
	if err == nil {
		t.Fatal("runContainer() error = nil, want provisioner error")
	}
	if result.operation.ErrorCode != chamberErrors.ErrInvalidBundleMount {
		t.Fatalf("operation ErrorCode = %q, want %q", result.operation.ErrorCode, chamberErrors.ErrInvalidBundleMount)
	}
}

func TestRunContainerRecordsPreciseRuntimeErrorCode(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)

	result, err := runContainer(
		context.Background(),
		store,
		fakeRuntime{err: fmt.Errorf("%w: launch canceled", chamberErrors.ErrCanceled)},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		context.Background(),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
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
		fakeRuntime{},
		fakeProvisioner{bundlePath: provisionedBundlePath},
		context.Background(),
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
		fakeRuntime{},
		fakeProvisioner{
			bundlePath: "/tmp/chamber-test/provisioner-owned/container",
			request:    &provisionRequest,
		},
		context.Background(),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
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

func putTestImage(t *testing.T, store metadata.Store) {
	t.Helper()

	if err := store.PutImage(context.Background(), metadata.Image{
		Reference:  "docker.io/library/alpine:latest",
		Digest:     "sha256:image",
		LayoutPath: "/tmp/chamber-test/images/alpine",
		PulledAt:   time.Now().UTC(),
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
	registerContainerRoutes(mux, store, nil, nil, context.Background())
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if recorder.Body.String() != "hello stderr" {
		t.Fatalf("log body = %q, want hello stderr", recorder.Body.String())
	}
}

func testConfig(t *testing.T) chamberDaemonConfig.Config {
	t.Helper()

	root := t.TempDir()
	return chamberDaemonConfig.Config{
		HTTPAddr: "127.0.0.1:0",
		Bundle: chamberBundle.Config{
			Root: root + "/bundles",
			Name: chamberBundle.ProvisionerNameDirectory,
		},
		Image: chamberImage.Config{
			Root: root + "/images",
		},
		Runtime: chamberRuntime.Config{
			RuntimeRoot: root + "/runtime",
		},
	}
}

type fakeProvisioner struct {
	bundlePath string
	err        error
	request    *chamberBundle.ProvisionRequest
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

type fakeRuntime struct {
	err error
}

func (r fakeRuntime) Descriptor() chamberRuntime.Descriptor {
	return chamberRuntime.Descriptor{Name: "fake"}
}

func (r fakeRuntime) Run(ctx context.Context, request chamberRuntime.RunRequest) (chamberRuntime.Container, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	return fakeContainer{
		id:         request.Bundle.ContainerID,
		stdoutPath: filepath.Join(os.TempDir(), request.Bundle.ContainerID, "stdout.log"),
		stderrPath: filepath.Join(os.TempDir(), request.Bundle.ContainerID, "stderr.log"),
	}, nil
}

type fakeContainer struct {
	id         string
	stdoutPath string
	stderrPath string
}

func (c fakeContainer) ID() string { return c.id }

func (c fakeContainer) StdoutPath() string { return c.stdoutPath }

func (c fakeContainer) StderrPath() string { return c.stderrPath }

func (fakeContainer) Wait() (chamberRuntime.ContainerResult, error) {
	return chamberRuntime.ContainerResult{}, nil
}

func (c fakeContainer) State(ctx context.Context) (chamberRuntime.ContainerState, error) {
	return chamberRuntime.ContainerState{ContainerID: c.id}, ctx.Err()
}

func (fakeContainer) Signal(ctx context.Context, signal os.Signal) error {
	return ctx.Err()
}

func (fakeContainer) Delete(ctx context.Context, force bool) error {
	return ctx.Err()
}

func (c fakeContainer) ReadLog(stream chamberRuntime.LogStream) ([]byte, error) {
	switch stream {
	case chamberRuntime.StdoutLogStream:
		return os.ReadFile(c.stdoutPath)
	case chamberRuntime.StderrLogStream:
		return os.ReadFile(c.stderrPath)
	default:
		return nil, chamberErrors.ErrInvalidRequest
	}
}

func (c fakeContainer) DeleteLog(stream chamberRuntime.LogStream) error {
	switch stream {
	case chamberRuntime.StdoutLogStream:
		return os.Remove(c.stdoutPath)
	case chamberRuntime.StderrLogStream:
		return os.Remove(c.stderrPath)
	default:
		return chamberErrors.ErrInvalidRequest
	}
}

type fakePuller struct {
	err error
}

func (p fakePuller) Pull(ctx context.Context, request chamberImage.PullRequest) (chamberImage.PulledImage, error) {
	if err := ctx.Err(); err != nil {
		return chamberImage.PulledImage{}, err
	}
	if p.err != nil {
		return chamberImage.PulledImage{}, p.err
	}
	return chamberImage.PulledImage{
		Reference:  request.Reference,
		Digest:     "sha256:abc123",
		LayoutPath: "/tmp/chamber-test/images/fake-layout",
		PulledAt:   time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}, nil
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
