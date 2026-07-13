package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/donglin-wang/chamber/daemon"
	"github.com/donglin-wang/chamber/internal/metadata"
)

type contextKey string

type fakeService struct {
	pullResult daemon.PullResult
	pullErr    error
	runResult  daemon.RunResult
	runErr     error
	listResult daemon.ListContainersResult
	listErr    error
	logResult  daemon.ContainerLogResult
	logErr     error

	pullCalls int
	runCalls  int
	listCalls int
	logCalls  int
	pullCtx   context.Context
	runCtx    context.Context
	listCtx   context.Context
	logCtx    context.Context
	pullReq   daemon.PullRequest
	runReq    daemon.RunRequest
	logID     string
	logStream string
}

func (s *fakeService) Pull(ctx context.Context, request daemon.PullRequest) (daemon.PullResult, error) {
	s.pullCalls++
	s.pullCtx = ctx
	s.pullReq = request
	return s.pullResult, s.pullErr
}

func (s *fakeService) Run(ctx context.Context, request daemon.RunRequest) (daemon.RunResult, error) {
	s.runCalls++
	s.runCtx = ctx
	s.runReq = request
	return s.runResult, s.runErr
}

func (s *fakeService) ListContainers(ctx context.Context) (daemon.ListContainersResult, error) {
	s.listCalls++
	s.listCtx = ctx
	return s.listResult, s.listErr
}

func (s *fakeService) ContainerLog(ctx context.Context, containerID string, stream string) (daemon.ContainerLogResult, error) {
	s.logCalls++
	s.logCtx = ctx
	s.logID = containerID
	s.logStream = stream
	return s.logResult, s.logErr
}

func TestNewHandlerServesDocsAndOpenAPI(t *testing.T) {
	service := &fakeService{}

	docs := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(docs, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if docs.Code != http.StatusOK {
		t.Fatalf("docs status = %d, want %d; body = %s", docs.Code, http.StatusOK, docs.Body.String())
	}
	if got := docs.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("docs Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if !strings.Contains(docs.Body.String(), `url: "/openapi.json"`) {
		t.Fatalf("docs body does not point Swagger UI at /openapi.json")
	}

	spec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(spec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if spec.Code != http.StatusOK {
		t.Fatalf("openapi status = %d, want %d; body = %s", spec.Code, http.StatusOK, spec.Body.String())
	}
	if got := spec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("openapi Content-Type = %q, want application/json", got)
	}
	var document map[string]any
	if err := json.NewDecoder(spec.Body).Decode(&document); err != nil {
		t.Fatalf("decode OpenAPI JSON: %v", err)
	}
	paths := document["paths"].(map[string]any)
	if _, ok := paths["/v1/images/pull"]; !ok {
		t.Fatalf("OpenAPI paths missing /v1/images/pull")
	}
	if _, ok := paths["/v1/containers/run"]; !ok {
		t.Fatalf("OpenAPI paths missing /v1/containers/run")
	}
	if _, ok := paths["/v1/containers"]; !ok {
		t.Fatalf("OpenAPI paths missing /v1/containers")
	}
	if _, ok := paths["/v1/containers/{id}/logs"]; !ok {
		t.Fatalf("OpenAPI paths missing /v1/containers/{id}/logs")
	}
	if service.pullCalls != 0 || service.runCalls != 0 || service.listCalls != 0 || service.logCalls != 0 {
		t.Fatalf("service calls = pull %d, run %d, list %d, log %d; want none", service.pullCalls, service.runCalls, service.listCalls, service.logCalls)
	}
}

func TestNewHandlerRejectsInvalidTransportRequestsBeforeService(t *testing.T) {
	tooLargeBody := `{"reference":"` + strings.Repeat("a", int(maxRequestBodyBytes)+1) + `"}`
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{
			name:   "wrong method",
			method: http.MethodGet,
			path:   "/v1/images/pull",
			body:   `{"reference":"docker.io/library/alpine:latest"}`,
			status: http.StatusMethodNotAllowed,
			code:   "method_not_allowed",
		},
		{
			name:   "wrong path",
			method: http.MethodPost,
			path:   "/v1/unknown",
			body:   `{}`,
			status: http.StatusNotFound,
			code:   "not_found",
		},
		{
			name:   "too large body",
			method: http.MethodPost,
			path:   "/v1/images/pull",
			body:   tooLargeBody,
			status: http.StatusBadRequest,
			code:   string(metadata.ErrInvalidRequest),
		},
		{
			name:   "malformed JSON",
			method: http.MethodPost,
			path:   "/v1/images/pull",
			body:   `{"reference":`,
			status: http.StatusBadRequest,
			code:   string(metadata.ErrInvalidRequest),
		},
		{
			name:   "second JSON value",
			method: http.MethodPost,
			path:   "/v1/images/pull",
			body:   `{"reference":"docker.io/library/alpine:latest"} {}`,
			status: http.StatusBadRequest,
			code:   string(metadata.ErrInvalidRequest),
		},
		{
			name:   "unknown field",
			method: http.MethodPost,
			path:   "/v1/images/pull",
			body:   `{"reference":"docker.io/library/alpine:latest","extra":true}`,
			status: http.StatusBadRequest,
			code:   string(metadata.ErrInvalidRequest),
		},
		{
			name:   "empty pull reference",
			method: http.MethodPost,
			path:   "/v1/images/pull",
			body:   `{"reference":"   "}`,
			status: http.StatusBadRequest,
			code:   string(metadata.ErrInvalidRequest),
		},
		{
			name:   "empty run image",
			method: http.MethodPost,
			path:   "/v1/containers/run",
			body:   `{"image":" ","command":["/bin/sh"]}`,
			status: http.StatusBadRequest,
			code:   string(metadata.ErrInvalidRequest),
		},
		{
			name:   "empty run command",
			method: http.MethodPost,
			path:   "/v1/containers/run",
			body:   `{"image":"docker.io/library/alpine:latest","command":[]}`,
			status: http.StatusBadRequest,
			code:   string(metadata.ErrInvalidRequest),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &fakeService{}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))

			NewHandler(service).ServeHTTP(recorder, request)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tt.status, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if got := recorder.Header().Get("X-Chamber-Operation-ID"); got != "" {
				t.Fatalf("X-Chamber-Operation-ID = %q, want empty", got)
			}
			if service.pullCalls != 0 || service.runCalls != 0 || service.listCalls != 0 || service.logCalls != 0 {
				t.Fatalf("service calls = pull %d, run %d, list %d, log %d; want none", service.pullCalls, service.runCalls, service.listCalls, service.logCalls)
			}

			var response ErrorResponse
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.Code != tt.code {
				t.Fatalf("error code = %q, want %q", response.Code, tt.code)
			}
			if response.OperationID != "" {
				t.Fatalf("operation id = %q, want empty", response.OperationID)
			}
		})
	}
}

func TestNewHandlerPullSuccessResponse(t *testing.T) {
	pulledAt := time.Date(2026, 7, 13, 12, 30, 0, 0, time.UTC)
	service := &fakeService{
		pullResult: daemon.PullResult{
			Operation: metadata.Operation{ID: "op-pull"},
			Image: metadata.Image{
				Reference: "docker.io/library/alpine:latest",
				Digest:    "sha256:abc123",
				PulledAt:  pulledAt,
			},
		},
	}
	recorder := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), contextKey("request-id"), "req-1")
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/images/pull",
		strings.NewReader(`{"reference":" docker.io/library/alpine:latest "}`),
	).WithContext(ctx)

	NewHandler(service).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := recorder.Header().Get("X-Chamber-Operation-ID"); got != "op-pull" {
		t.Fatalf("X-Chamber-Operation-ID = %q, want op-pull", got)
	}
	if service.pullCalls != 1 {
		t.Fatalf("Pull calls = %d, want 1", service.pullCalls)
	}
	if got := service.pullReq.Reference; got != "docker.io/library/alpine:latest" {
		t.Fatalf("Pull reference = %q, want trimmed reference", got)
	}
	if got := service.pullCtx.Value(contextKey("request-id")); got != "req-1" {
		t.Fatalf("context value = %v, want req-1", got)
	}

	var response PullResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if response.OperationID != "op-pull" ||
		response.Reference != "docker.io/library/alpine:latest" ||
		response.Digest != "sha256:abc123" ||
		!response.PulledAt.Equal(pulledAt) {
		t.Fatalf("response = %#v, want pull result fields", response)
	}
}

func TestNewHandlerRunSuccessResponse(t *testing.T) {
	service := &fakeService{
		runResult: daemon.RunResult{
			Operation: metadata.Operation{ID: "op-run"},
			Container: metadata.Container{
				ID:          "ctr-run",
				ImageDigest: "sha256:def456",
				State:       metadata.ContainerRunning,
			},
		},
	}
	recorder := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), contextKey("request-id"), "req-2")
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/containers/run",
		strings.NewReader(`{"image":" docker.io/library/alpine:latest ","command":["/bin/sh","-c","echo hello"]}`),
	).WithContext(ctx)

	NewHandler(service).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := recorder.Header().Get("X-Chamber-Operation-ID"); got != "op-run" {
		t.Fatalf("X-Chamber-Operation-ID = %q, want op-run", got)
	}
	if service.runCalls != 1 {
		t.Fatalf("Run calls = %d, want 1", service.runCalls)
	}
	if got := service.runReq.Image; got != "docker.io/library/alpine:latest" {
		t.Fatalf("Run image = %q, want trimmed image", got)
	}
	if got := service.runReq.Command; len(got) != 3 || got[0] != "/bin/sh" || got[2] != "echo hello" {
		t.Fatalf("Run command = %#v, want request command", got)
	}
	if got := service.runCtx.Value(contextKey("request-id")); got != "req-2" {
		t.Fatalf("context value = %v, want req-2", got)
	}

	var response RunResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	if response.OperationID != "op-run" ||
		response.ID != "ctr-run" ||
		response.ImageDigest != "sha256:def456" ||
		response.State != metadata.ContainerRunning {
		t.Fatalf("response = %#v, want run result fields", response)
	}
}

func TestNewHandlerListContainersSuccessResponse(t *testing.T) {
	exitCode := 0
	createdAt := time.Date(2026, 7, 13, 12, 30, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Second)
	service := &fakeService{
		listResult: daemon.ListContainersResult{
			Containers: []metadata.Container{
				{
					ID:          "ctr-run",
					OperationID: "op-run",
					ImageDigest: "sha256:def456",
					ImageRef:    "docker.io/library/alpine:latest",
					Runtime:     "runc",
					State:       metadata.ContainerExited,
					CreatedAt:   createdAt,
					UpdatedAt:   updatedAt,
					ExitCode:    &exitCode,
				},
			},
		},
	}
	recorder := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), contextKey("request-id"), "req-list")
	request := httptest.NewRequest(http.MethodGet, "/v1/containers", nil).WithContext(ctx)

	NewHandler(service).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if service.listCalls != 1 {
		t.Fatalf("ListContainers calls = %d, want 1", service.listCalls)
	}
	if got := service.listCtx.Value(contextKey("request-id")); got != "req-list" {
		t.Fatalf("context value = %v, want req-list", got)
	}

	var response ListContainersResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(response.Containers) != 1 {
		t.Fatalf("containers len = %d, want 1", len(response.Containers))
	}
	got := response.Containers[0]
	if got.ID != "ctr-run" ||
		got.OperationID != "op-run" ||
		got.Image != "docker.io/library/alpine:latest" ||
		got.ImageDigest != "sha256:def456" ||
		got.Runtime != "runc" ||
		got.State != metadata.ContainerExited ||
		got.ExitCode == nil ||
		*got.ExitCode != 0 {
		t.Fatalf("container response = %#v, want container fields", got)
	}
}

func TestNewHandlerContainerLogSuccessResponse(t *testing.T) {
	service := &fakeService{
		logResult: daemon.ContainerLogResult{
			Container: metadata.Container{ID: "ctr-run"},
			Stream:    "stderr",
			Content:   []byte("hello stderr\n"),
		},
	}
	recorder := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), contextKey("request-id"), "req-log")
	request := httptest.NewRequest(http.MethodGet, "/v1/containers/ctr-run/logs?stream=stderr", nil).WithContext(ctx)

	NewHandler(service).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := recorder.Header().Get("X-Chamber-Container-ID"); got != "ctr-run" {
		t.Fatalf("X-Chamber-Container-ID = %q, want ctr-run", got)
	}
	if got := recorder.Header().Get("X-Chamber-Log-Stream"); got != "stderr" {
		t.Fatalf("X-Chamber-Log-Stream = %q, want stderr", got)
	}
	if recorder.Body.String() != "hello stderr\n" {
		t.Fatalf("body = %q, want log content", recorder.Body.String())
	}
	if service.logCalls != 1 || service.logID != "ctr-run" || service.logStream != "stderr" {
		t.Fatalf("log call = calls %d id %q stream %q; want ctr-run stderr", service.logCalls, service.logID, service.logStream)
	}
	if got := service.logCtx.Value(contextKey("request-id")); got != "req-log" {
		t.Fatalf("context value = %v, want req-log", got)
	}
}

func TestNewHandlerMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		status      int
		code        string
		message     string
		operationID string
	}{
		{
			name:        "daemon invalid request",
			err:         &daemon.Error{OperationID: "op-invalid", Code: string(metadata.ErrInvalidRequest), Err: errors.New("internal validation detail")},
			status:      http.StatusBadRequest,
			code:        string(metadata.ErrInvalidRequest),
			message:     "invalid request",
			operationID: "op-invalid",
		},
		{
			name:        "image not found",
			err:         &daemon.Error{OperationID: "op-missing", Code: string(metadata.ErrImageNotFound), Err: errors.New("metadata: not found")},
			status:      http.StatusNotFound,
			code:        string(metadata.ErrImageNotFound),
			message:     "image not found",
			operationID: "op-missing",
		},
		{
			name:    "container not found",
			err:     &daemon.Error{Code: string(metadata.ErrContainerNotFound), Err: errors.New("metadata: not found")},
			status:  http.StatusNotFound,
			code:    string(metadata.ErrContainerNotFound),
			message: "container not found",
		},
		{
			name:    "container log not found",
			err:     &daemon.Error{Code: string(metadata.ErrLogNotFound), Err: errors.New("missing log")},
			status:  http.StatusNotFound,
			code:    string(metadata.ErrLogNotFound),
			message: "container log not found",
		},
		{
			name:        "daemon state conflict",
			err:         &daemon.Error{OperationID: "op-conflict", Code: string(metadata.ErrStateConflict), Err: errors.New("cas failed")},
			status:      http.StatusConflict,
			code:        "conflict",
			message:     "operation conflict",
			operationID: "op-conflict",
		},
		{
			name:    "metadata state conflict",
			err:     metadata.ErrStateConflict,
			status:  http.StatusConflict,
			code:    "conflict",
			message: "operation conflict",
		},
		{
			name:        "duplicate record conflict",
			err:         &daemon.Error{OperationID: "op-duplicate", Code: string(metadata.ErrMetadataFailed), Err: metadata.ErrAlreadyExists},
			status:      http.StatusConflict,
			code:        "conflict",
			message:     "operation conflict",
			operationID: "op-duplicate",
		},
		{
			name:        "pull failure",
			err:         &daemon.Error{OperationID: "op-pull", Code: string(metadata.ErrPullFailed), Err: errors.New("registry said no")},
			status:      http.StatusInternalServerError,
			code:        string(metadata.ErrPullFailed),
			message:     "internal error",
			operationID: "op-pull",
		},
		{
			name:        "store failure",
			err:         &daemon.Error{OperationID: "op-store", Code: string(metadata.ErrMetadataFailed), Err: errors.New("etcd unavailable")},
			status:      http.StatusInternalServerError,
			code:        string(metadata.ErrMetadataFailed),
			message:     "internal error",
			operationID: "op-store",
		},
		{
			name:        "bundle failure",
			err:         &daemon.Error{OperationID: "op-bundle", Code: string(metadata.ErrBundlePrepareFailed), Err: errors.New("umoci failed")},
			status:      http.StatusInternalServerError,
			code:        string(metadata.ErrBundlePrepareFailed),
			message:     "internal error",
			operationID: "op-bundle",
		},
		{
			name:        "runtime failure",
			err:         &daemon.Error{OperationID: "op-runtime", Code: string(metadata.ErrRuntimeStartFailed), Err: errors.New("runc failed")},
			status:      http.StatusInternalServerError,
			code:        string(metadata.ErrRuntimeStartFailed),
			message:     "internal error",
			operationID: "op-runtime",
		},
		{
			name:        "plain unexpected error",
			err:         errors.New("plain internal detail"),
			status:      http.StatusInternalServerError,
			code:        "internal",
			message:     "internal error",
			operationID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &fakeService{runErr: tt.err}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/containers/run",
				strings.NewReader(`{"image":"docker.io/library/alpine:latest","command":["/bin/sh"]}`),
			)

			NewHandler(service).ServeHTTP(recorder, request)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tt.status, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if got := recorder.Header().Get("X-Chamber-Operation-ID"); got != tt.operationID {
				t.Fatalf("X-Chamber-Operation-ID = %q, want %q", got, tt.operationID)
			}

			var response ErrorResponse
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.Code != tt.code || response.Message != tt.message || response.OperationID != tt.operationID {
				t.Fatalf("response = %#v, want code %q message %q operation %q", response, tt.code, tt.message, tt.operationID)
			}
			if strings.Contains(recorder.Body.String(), "internal detail") || strings.Contains(recorder.Body.String(), "runc failed") {
				t.Fatalf("response leaked internal error detail: %s", recorder.Body.String())
			}
		})
	}
}
