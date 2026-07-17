package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chamberDaemonConfig "github.com/donglin-wang/chamber/daemon/config"
	"github.com/donglin-wang/chamber/daemon/metadata"
	"github.com/donglin-wang/chamber/daemon/metadata/memory"
	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
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

func TestRunContainerRequiresCommand(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/containers/run", strings.NewReader(`{"image":"docker.io/library/alpine:latest","command":[]}`))

	mux := newServer()
	registerContainerRoutes(mux, testConfig(t), memory.NewMemoryStore(), nil, nil, context.Background())
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestRunContainerStoresProvisionedBundlePath(t *testing.T) {
	store := memory.NewMemoryStore()
	if err := store.PutImage(context.Background(), metadata.Image{
		Reference:  "docker.io/library/alpine:latest",
		Digest:     "sha256:image",
		LayoutPath: "/tmp/chamber-test/images/alpine",
		PulledAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutImage() error = %v", err)
	}

	provisionedBundlePath := "/tmp/chamber-test/provisioner-owned/container"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/containers/run", strings.NewReader(`{
		"image":"docker.io/library/alpine:latest",
		"command":["/bin/true"]
	}`))

	mux := newServer()
	registerContainerRoutes(
		mux,
		testConfig(t),
		store,
		fakeRuntime{t: t},
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
}

func TestContainerLogsReadByContainerID(t *testing.T) {
	store := memory.NewMemoryStore()
	container := metadata.Container{
		ID:          "container-1",
		OperationID: "operation-1",
		ImageRef:    "docker.io/library/alpine:latest",
		ImageDigest: "sha256:image",
		BundlePath:  "/tmp/chamber-test/not-a-log-location",
		Runtime:     chamberRuntime.DefaultName,
		State:       metadata.ContainerExited,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := store.CreateContainer(context.Background(), container); err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}

	runtime := fakeRuntime{
		t:       t,
		logs:    map[string][]byte{"container-1:stderr": []byte("hello stderr")},
		wantLog: "container-1:stderr",
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/containers/container-1/logs?stream=stderr", nil)

	mux := newServer()
	registerContainerRoutes(mux, testConfig(t), store, runtime, nil, context.Background())
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
}

func (p fakeProvisioner) Provision(ctx context.Context, request chamberBundle.ProvisionRequest) (chamberBundle.ProvisionedBundle, error) {
	if err := ctx.Err(); err != nil {
		return chamberBundle.ProvisionedBundle{}, err
	}
	return chamberBundle.ProvisionedBundle{
		ContainerID: request.ContainerID,
		BundlePath:  p.bundlePath,
	}, nil
}

type fakeRuntime struct {
	t       *testing.T
	logs    map[string][]byte
	wantLog string
}

func (r fakeRuntime) Ensure(ctx context.Context) (chamberRuntime.Binary, error) {
	return chamberRuntime.Binary{}, ctx.Err()
}

func (r fakeRuntime) Run(ctx context.Context, request chamberRuntime.RunRequest) (chamberRuntime.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return fakeProcess{}, nil
}

func (r fakeRuntime) State(ctx context.Context, containerID string) (chamberRuntime.ContainerState, error) {
	return chamberRuntime.ContainerState{}, ctx.Err()
}

func (r fakeRuntime) Signal(ctx context.Context, request chamberRuntime.SignalRequest) error {
	return ctx.Err()
}

func (r fakeRuntime) Delete(ctx context.Context, request chamberRuntime.DeleteRequest) error {
	return ctx.Err()
}

func (r fakeRuntime) ReadLog(containerID string, stream string) ([]byte, error) {
	key := containerID + ":" + stream
	if r.wantLog != "" && key != r.wantLog {
		r.t.Fatalf("ReadLog key = %q, want %q", key, r.wantLog)
	}
	return r.logs[key], nil
}

type fakeProcess struct{}

func (fakeProcess) Wait() (int, error) {
	return 0, nil
}

type fakePuller struct{}

func (fakePuller) Pull(ctx context.Context, request chamberImage.PullRequest) (chamberImage.PulledImage, error) {
	if err := ctx.Err(); err != nil {
		return chamberImage.PulledImage{}, err
	}
	return chamberImage.PulledImage{
		Reference:  request.Reference,
		Digest:     "sha256:abc123",
		LayoutPath: request.Destination,
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
