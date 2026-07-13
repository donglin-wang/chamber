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

	pullCalls int
	runCalls  int
	pullCtx   context.Context
	runCtx    context.Context
	pullReq   daemon.PullRequest
	runReq    daemon.RunRequest
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
			if service.pullCalls != 0 || service.runCalls != 0 {
				t.Fatalf("service calls = pull %d, run %d; want none", service.pullCalls, service.runCalls)
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
			code:        "internal",
			message:     "internal error",
			operationID: "op-pull",
		},
		{
			name:        "store failure",
			err:         &daemon.Error{OperationID: "op-store", Code: string(metadata.ErrMetadataFailed), Err: errors.New("etcd unavailable")},
			status:      http.StatusInternalServerError,
			code:        "internal",
			message:     "internal error",
			operationID: "op-store",
		},
		{
			name:        "bundle failure",
			err:         &daemon.Error{OperationID: "op-bundle", Code: string(metadata.ErrBundlePrepareFailed), Err: errors.New("umoci failed")},
			status:      http.StatusInternalServerError,
			code:        "internal",
			message:     "internal error",
			operationID: "op-bundle",
		},
		{
			name:        "runtime failure",
			err:         &daemon.Error{OperationID: "op-runtime", Code: string(metadata.ErrRuntimeStartFailed), Err: errors.New("runc failed")},
			status:      http.StatusInternalServerError,
			code:        "internal",
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
