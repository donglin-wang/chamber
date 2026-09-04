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
	"sync/atomic"
	"syscall"
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
		nil,
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
		nil,
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
		nil,
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
		nil,
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
		nil,
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

func TestRunContainerSurvivesCanceledClientContextAfterAdmission(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/containers/run", strings.NewReader(`{
		"image":"docker.io/library/alpine:latest",
		"command":["/bin/true"]
	}`)).WithContext(ctx)

	mux := newServer()
	registerContainerRoutes(
		mux,
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake"},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		t.TempDir(),
		fakeStartSupervisor(nil),
		fakeOpenContainer(nil),
		nil,
	)
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
}

func TestCreateContainerPreparesBundleWithoutStarting(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)

	result, err := createContainer(
		context.Background(),
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake", RuntimeRoot: "/tmp/chamber-test/runtime"},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		t.TempDir(),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
		nil,
	)
	if err != nil {
		t.Fatalf("createContainer() error = %v", err)
	}
	if result.operation.Kind != metadata.CreateOperation || result.operation.State != metadata.OperationSucceeded {
		t.Fatalf("create operation = %#v, want succeeded create operation", result.operation)
	}
	if result.container.State != metadata.ContainerCreated {
		t.Fatalf("container state = %q, want %q", result.container.State, metadata.ContainerCreated)
	}
	state, err := readRequiredSupervisorFile(result.container.SupervisorPath)
	if err != nil {
		t.Fatalf("readRequiredSupervisorFile() error = %v", err)
	}
	if state.Phase != supervisorPrepared || state.OperationID != result.operation.ID {
		t.Fatalf("supervisor state = %#v, want prepared for create operation", state)
	}
}

func TestStartContainerTakesLifecycleOwnershipAndLaunchesSupervisor(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)
	created, err := createContainer(
		context.Background(),
		store,
		fakeImageStore{},
		chamberRuntime.Config{Name: "fake", RuntimeRoot: "/tmp/chamber-test/runtime"},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		t.TempDir(),
		"docker.io/library/alpine:latest",
		[]string{"/bin/true"},
		nil,
	)
	if err != nil {
		t.Fatalf("createContainer() error = %v", err)
	}

	started, err := startContainer(
		context.Background(),
		store,
		fakeStartSupervisor(nil),
		nil,
		created.container.ID,
	)
	if err != nil {
		t.Fatalf("startContainer() error = %v", err)
	}
	if started.operation.Kind != metadata.StartOperation || started.operation.State != metadata.OperationRunning {
		t.Fatalf("start operation = %#v, want running start operation", started.operation)
	}
	if started.operation.ID == created.operation.ID {
		t.Fatalf("start operation ID = create operation ID %q, want independent lifecycle operation", created.operation.ID)
	}
	if started.container.State != metadata.ContainerStarting {
		t.Fatalf("container state = %q, want %q", started.container.State, metadata.ContainerStarting)
	}
	if started.container.OperationID != started.operation.ID {
		t.Fatalf("container operation ID = %q, want start operation %q", started.container.OperationID, started.operation.ID)
	}
	state, err := readRequiredSupervisorFile(started.container.SupervisorPath)
	if err != nil {
		t.Fatalf("readRequiredSupervisorFile() error = %v", err)
	}
	if state.Phase != supervisorPrepared || state.OperationID != started.operation.ID {
		t.Fatalf("supervisor state = %#v, want prepared for start operation", state)
	}
}

func TestStartContainerLaunchFailureSchedulesDurableCleanup(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)
	created, err := createContainer(
		context.Background(), store, fakeImageStore{},
		chamberRuntime.Config{Name: "fake", RuntimeRoot: "/tmp/chamber-test/runtime"},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		t.TempDir(), "docker.io/library/alpine:latest", []string{"/bin/true"}, nil,
	)
	if err != nil {
		t.Fatalf("createContainer() error = %v", err)
	}

	result, err := startContainer(context.Background(), store, fakeStartSupervisor(errors.New("launch failed")), nil, created.container.ID)
	if err == nil {
		t.Fatal("startContainer() error = nil, want launch failure")
	}
	if result.container.State != metadata.ContainerFailed || result.operation.State != metadata.OperationFailed {
		t.Fatalf("failed start result = %#v", result)
	}
	cleanups, listErr := store.ListCleanups(context.Background())
	if listErr != nil || len(cleanups) != 1 || cleanups[0].ContainerID != created.container.ID {
		t.Fatalf("ListCleanups() = %#v, %v; want durable cleanup", cleanups, listErr)
	}
}

func TestPrepareContainerSupervisorWriteFailureSchedulesDurableCleanup(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)
	supervisorRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(supervisorRoot, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(supervisor root) error = %v", err)
	}
	operation, containerID, err := newContainerAdmission(metadata.RunOperation)
	if err != nil {
		t.Fatalf("newContainerAdmission() error = %v", err)
	}

	result, err := prepareContainer(
		context.Background(), store, fakeImageStore{},
		chamberRuntime.Config{Name: "fake", RuntimeRoot: "/tmp/chamber-test/runtime"},
		fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
		supervisorRoot, operation, containerID, "docker.io/library/alpine:latest", []string{"/bin/true"}, nil,
	)
	if err == nil {
		t.Fatal("prepareContainer() error = nil, want supervisor write failure")
	}
	if result.container.State != metadata.ContainerFailed || result.operation.State != metadata.OperationFailed {
		t.Fatalf("failed prepare result = %#v", result)
	}
	cleanups, listErr := store.ListCleanups(context.Background())
	if listErr != nil || len(cleanups) != 1 || cleanups[0].ContainerID != result.container.ID {
		t.Fatalf("ListCleanups() = %#v, %v; want durable cleanup", cleanups, listErr)
	}
}

func TestRunContainerHoldsContainerLockFromPreparationThroughLaunch(t *testing.T) {
	store := memory.NewMemoryStore()
	putTestImage(t, store)
	pauseDir := t.TempDir()
	t.Setenv(daemonLifecyclePauseDirEnv, pauseDir)
	t.Setenv(daemonLifecyclePausePointEnv, daemonPauseAfterPrepared)

	var starts atomic.Int32
	startSupervisor := func(_ context.Context, path string) (int, func() error, error) {
		starts.Add(1)
		if err := markSupervisorLaunched(path, 1234, 77, time.Now().UTC()); err != nil {
			return 0, nil, err
		}
		if err := markSupervisorStarted(path, "fake", time.Now().UTC()); err != nil {
			return 0, nil, err
		}
		return 1234, nil, nil
	}
	type result struct {
		run runContainerResult
		err error
	}
	runDone := make(chan result, 1)
	go func() {
		run, err := runContainer(
			context.Background(), store, fakeImageStore{},
			chamberRuntime.Config{Name: "fake", RuntimeRoot: "/tmp/chamber-test/runtime"},
			fakeProvisioner{bundlePath: "/tmp/chamber-test/provisioner-owned/container"},
			t.TempDir(), startSupervisor, nil,
			"docker.io/library/alpine:latest", []string{"/bin/true"}, nil,
		)
		runDone <- result{run: run, err: err}
	}()

	var signalPath string
	deadline := time.Now().Add(2 * time.Second)
	for signalPath == "" && time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(pauseDir, "*."+daemonPauseAfterPrepared+".json"))
		if err != nil {
			t.Fatalf("Glob(pause evidence) error = %v", err)
		}
		if len(matches) > 0 {
			signalPath = matches[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if signalPath == "" {
		t.Fatal("prepared pause evidence was not written")
	}

	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- reconcileSupervisorContainers(context.Background(), store, startSupervisor, nil, chamberRuntime.Config{Name: "fake"}, false)
	}()
	select {
	case err := <-reconcileDone:
		t.Fatalf("reconciliation escaped the active container lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	releasePath := strings.TrimSuffix(signalPath, ".json") + ".release"
	if err := os.WriteFile(releasePath, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(release) error = %v", err)
	}
	completed := <-runDone
	if completed.err != nil {
		t.Fatalf("runContainer() error = %v", completed.err)
	}
	if err := <-reconcileDone; err != nil {
		t.Fatalf("reconcileSupervisorContainers() error = %v", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("supervisor starts = %d, want 1", starts.Load())
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
	var terminatedStartTime uint64

	result, err := cancelContainer(
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
		func(pid int, startTime uint64) error {
			terminatedPID = pid
			terminatedStartTime = startTime
			return nil
		},
		container.ID,
	)
	if err != nil {
		t.Fatalf("cancelContainer() error = %v", err)
	}
	canceled := result.container
	if result.operation.Kind != metadata.CancelOperation || result.operation.State != metadata.OperationSucceeded {
		t.Fatalf("cancel operation = %#v, want succeeded cancel operation", result.operation)
	}
	if result.operation.ID == container.OperationID {
		t.Fatalf("cancel operation ID = run operation ID %q, want independent lifecycle operation", container.OperationID)
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
	if terminatedStartTime != container.SupervisorStartTime {
		t.Fatalf("terminated supervisor start time = %d, want %d", terminatedStartTime, container.SupervisorStartTime)
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

func TestStopContainerSignalsRuntimeWithoutCleanup(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)
	handle := &fakeContainerHandle{id: container.ID}
	var openedConfig chamberRuntime.Config

	stopped, err := stopContainer(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake", RuntimeRoot: "/runtime/from-config"},
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
		container.ID,
	)
	if err != nil {
		t.Fatalf("stopContainer() error = %v", err)
	}
	if stopped.operation.Kind != metadata.StopOperation || stopped.operation.State != metadata.OperationSucceeded {
		t.Fatalf("stop operation = %#v, want succeeded stop operation", stopped.operation)
	}
	if stopped.operation.ID == container.OperationID {
		t.Fatalf("stop operation ID = run operation ID %q, want independent lifecycle operation", container.OperationID)
	}
	if openedConfig.RuntimeRoot != container.RuntimeRoot {
		t.Fatalf("runtime root = %q, want persisted container runtime root %q", openedConfig.RuntimeRoot, container.RuntimeRoot)
	}
	if handle.signal != syscall.SIGTERM {
		t.Fatalf("signal = %v, want SIGTERM", handle.signal)
	}
	if handle.deleted || handle.deletedStdout || handle.deletedStderr {
		t.Fatalf("runtime cleanup handle = %#v, want signal only", handle)
	}
	if stopped.container.State != metadata.ContainerRunning {
		t.Fatalf("container state = %q, want unchanged running state", stopped.container.State)
	}
	operation, err := store.GetOperation(context.Background(), stopped.operation.ID)
	if err != nil {
		t.Fatalf("GetOperation(stop) error = %v", err)
	}
	if operation.State != metadata.OperationSucceeded {
		t.Fatalf("stored stop operation state = %q, want succeeded", operation.State)
	}
}

func TestStopContainerRejectsCreatingContainer(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerCreating)

	_, err := stopContainer(
		context.Background(),
		store,
		chamberRuntime.Config{Name: "fake"},
		fakeOpenContainer(nil),
		container.ID,
	)
	if err == nil {
		t.Fatal("stopContainer(creating) error = nil, want state conflict")
	}
	if !errors.Is(err, chamberErrors.ErrStateConflict) {
		t.Fatalf("stopContainer(creating) error = %v, want state conflict", err)
	}
}

func TestStopContainerRouteReturnsStopOperationHeader(t *testing.T) {
	store := memory.NewMemoryStore()
	container := createTestContainer(t, store, metadata.ContainerRunning)
	handle := &fakeContainerHandle{id: container.ID}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/containers/"+container.ID+"/stop", strings.NewReader(`{}`))

	mux := newServer()
	registerContainerRoutes(
		mux,
		store,
		nil,
		chamberRuntime.Config{Name: "fake"},
		nil,
		t.TempDir(),
		fakeStartSupervisor(nil),
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
	)
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	stopOperationID := recorder.Header().Get("X-Chamber-Operation-ID")
	if stopOperationID == "" {
		t.Fatal("X-Chamber-Operation-ID = empty, want stop operation id")
	}
	if stopOperationID == container.OperationID {
		t.Fatalf("X-Chamber-Operation-ID = run operation ID %q, want stop operation id", stopOperationID)
	}
	var response containerResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode stop response: %v", err)
	}
	if response.OperationID != container.OperationID {
		t.Fatalf("response execution operation id = %q, want %q", response.OperationID, container.OperationID)
	}
	if response.OperationID == stopOperationID {
		t.Fatalf("response operation id should remain execution ownership, not stop operation %q", stopOperationID)
	}
	operation, err := store.GetOperation(context.Background(), stopOperationID)
	if err != nil {
		t.Fatalf("GetOperation(stop) error = %v", err)
	}
	if operation.Kind != metadata.StopOperation || operation.ResourceID != container.ID {
		t.Fatalf("stop operation = %#v, want stop operation for container", operation)
	}
	if handle.signal != syscall.SIGTERM {
		t.Fatalf("signal = %v, want SIGTERM", handle.signal)
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

	result, err := removeContainer(
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
	removedContainer := result.container
	if result.operation.Kind != metadata.RemoveOperation || result.operation.State != metadata.OperationSucceeded {
		t.Fatalf("remove operation = %#v, want succeeded remove operation", result.operation)
	}
	if result.operation.ID == container.OperationID {
		t.Fatalf("remove operation ID = run operation ID %q, want independent lifecycle operation", container.OperationID)
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
		ID:                  operation.ResourceID,
		OperationID:         operation.ID,
		ImageRef:            "docker.io/library/alpine:latest",
		ImageDigest:         "sha256:image",
		BundlePath:          "/tmp/chamber-test/bundles/" + operation.ResourceID,
		Runtime:             "fake",
		RuntimeRoot:         "/tmp/chamber-test/runtime",
		SupervisorPath:      "/tmp/chamber-test/supervisors/" + operation.ResourceID + "/supervisor.json",
		SupervisorPID:       1234,
		SupervisorStartTime: 77,
		State:               state,
		CreatedAt:           now,
		UpdatedAt:           now,
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
	signal         os.Signal
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

func (h *fakeContainerHandle) Signal(_ context.Context, signal os.Signal) error {
	h.signal = signal
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
