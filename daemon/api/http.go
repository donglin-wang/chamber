package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/donglin-wang/chamber/daemon"
	"github.com/donglin-wang/chamber/internal/metadata"
)

const maxRequestBodyBytes int64 = 1 << 20

type PullRequest struct {
	Reference string `json:"reference"`
}

type PullResponse struct {
	OperationID string    `json:"operation_id"`
	Reference   string    `json:"reference"`
	Digest      string    `json:"digest"`
	PulledAt    time.Time `json:"pulled_at"`
}

type RunRequest struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
}

type RunResponse struct {
	OperationID string                  `json:"operation_id"`
	ID          string                  `json:"id"`
	ImageDigest string                  `json:"image_digest"`
	State       metadata.ContainerState `json:"state"`
}

type ListContainersResponse struct {
	Containers []ContainerResponse `json:"containers"`
}

type ContainerResponse struct {
	ID          string                  `json:"id"`
	OperationID string                  `json:"operation_id"`
	Image       string                  `json:"image"`
	ImageDigest string                  `json:"image_digest"`
	Runtime     string                  `json:"runtime"`
	State       metadata.ContainerState `json:"state"`
	CreatedAt   time.Time               `json:"created_at"`
	UpdatedAt   time.Time               `json:"updated_at"`
	ExitCode    *int                    `json:"exit_code,omitempty"`
	ErrorCode   metadata.ErrorCode      `json:"error_code,omitempty"`
}

type ErrorResponse struct {
	OperationID string `json:"operation_id,omitempty"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

type Service interface {
	Pull(ctx context.Context, request daemon.PullRequest) (daemon.PullResult, error)
	Run(ctx context.Context, request daemon.RunRequest) (daemon.RunResult, error)
	ListContainers(ctx context.Context) (daemon.ListContainersResult, error)
	ContainerLog(ctx context.Context, containerID string, stream string) (daemon.ContainerLogResult, error)
}

func NewHandler(service Service) http.Handler {
	return &handler{service: service}
}

type handler struct {
	service Service
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/docs", "/swagger":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "", "method_not_allowed", "method not allowed")
			return
		}
		serveDocs(w, r)
	case "/openapi.json":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "", "method_not_allowed", "method not allowed")
			return
		}
		serveOpenAPI(w, r)
	case "/v1/images/pull":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "", "method_not_allowed", "method not allowed")
			return
		}
		h.pull(w, r)
	case "/v1/containers/run":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "", "method_not_allowed", "method not allowed")
			return
		}
		h.run(w, r)
	case "/v1/containers":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "", "method_not_allowed", "method not allowed")
			return
		}
		h.listContainers(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/v1/containers/") && strings.HasSuffix(r.URL.Path, "/logs") {
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, "", "method_not_allowed", "method not allowed")
				return
			}
			h.containerLog(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "", "not_found", "not found")
	}
}

func (h *handler) pull(w http.ResponseWriter, r *http.Request) {
	var request PullRequest
	if err := decodeStrictJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "", string(metadata.ErrInvalidRequest), "invalid request")
		return
	}
	request.Reference = strings.TrimSpace(request.Reference)
	if request.Reference == "" {
		writeError(w, http.StatusBadRequest, "", string(metadata.ErrInvalidRequest), "invalid request")
		return
	}

	result, err := h.service.Pull(r.Context(), daemon.PullRequest{
		Reference: request.Reference,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, result.Operation.ID, PullResponse{
		OperationID: result.Operation.ID,
		Reference:   result.Image.Reference,
		Digest:      result.Image.Digest,
		PulledAt:    result.Image.PulledAt,
	})
}

func (h *handler) run(w http.ResponseWriter, r *http.Request) {
	var request RunRequest
	if err := decodeStrictJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "", string(metadata.ErrInvalidRequest), "invalid request")
		return
	}
	request.Image = strings.TrimSpace(request.Image)
	if request.Image == "" || len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		writeError(w, http.StatusBadRequest, "", string(metadata.ErrInvalidRequest), "invalid request")
		return
	}

	result, err := h.service.Run(r.Context(), daemon.RunRequest{
		Image:   request.Image,
		Command: request.Command,
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, result.Operation.ID, RunResponse{
		OperationID: result.Operation.ID,
		ID:          result.Container.ID,
		ImageDigest: result.Container.ImageDigest,
		State:       result.Container.State,
	})
}

func (h *handler) listContainers(w http.ResponseWriter, r *http.Request) {
	result, err := h.service.ListContainers(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	containers := make([]ContainerResponse, 0, len(result.Containers))
	for _, container := range result.Containers {
		containers = append(containers, containerResponse(container))
	}
	writeJSON(w, http.StatusOK, "", ListContainersResponse{Containers: containers})
}

func (h *handler) containerLog(w http.ResponseWriter, r *http.Request) {
	containerID, ok := logContainerID(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "", "not_found", "not found")
		return
	}
	stream := r.URL.Query().Get("stream")
	result, err := h.service.ContainerLog(r.Context(), containerID, stream)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Chamber-Container-ID", result.Container.ID)
	w.Header().Set("X-Chamber-Log-Stream", result.Stream)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Content)
}

func logContainerID(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/v1/containers/")
	containerID, suffix, ok := strings.Cut(rest, "/")
	if !ok || suffix != "logs" || strings.TrimSpace(containerID) == "" || strings.Contains(containerID, "/") {
		return "", false
	}
	return containerID, true
}

func containerResponse(container metadata.Container) ContainerResponse {
	return ContainerResponse{
		ID:          container.ID,
		OperationID: container.OperationID,
		Image:       container.ImageRef,
		ImageDigest: container.ImageDigest,
		Runtime:     container.Runtime,
		State:       container.State,
		CreatedAt:   container.CreatedAt,
		UpdatedAt:   container.UpdatedAt,
		ExitCode:    container.ExitCode,
		ErrorCode:   container.ErrorCode,
	}
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeServiceError(w http.ResponseWriter, err error) {
	var daemonErr *daemon.Error
	operationID := ""
	if errors.As(err, &daemonErr) {
		operationID = daemonErr.OperationID
	}

	status, code, message := publicError(err, daemonErr)
	if status == http.StatusInternalServerError {
		slog.Default().Error("daemon request failed", "operation_id", operationID, "code", code)
	}
	writeError(w, status, operationID, code, message)
}

func publicError(err error, daemonErr *daemon.Error) (int, string, string) {
	code := ""
	if daemonErr != nil {
		code = daemonErr.Code
	}

	switch {
	case code == string(metadata.ErrInvalidRequest), errors.Is(err, metadata.ErrInvalidRequest):
		return http.StatusBadRequest, string(metadata.ErrInvalidRequest), "invalid request"
	case code == string(metadata.ErrImageNotFound), errors.Is(err, metadata.ErrImageNotFound):
		return http.StatusNotFound, string(metadata.ErrImageNotFound), "image not found"
	case code == string(metadata.ErrContainerNotFound), errors.Is(err, metadata.ErrContainerNotFound):
		return http.StatusNotFound, string(metadata.ErrContainerNotFound), "container not found"
	case code == string(metadata.ErrLogNotFound), errors.Is(err, metadata.ErrLogNotFound):
		return http.StatusNotFound, string(metadata.ErrLogNotFound), "container log not found"
	case code == string(metadata.ErrStateConflict),
		errors.Is(err, metadata.ErrStateConflict),
		errors.Is(err, metadata.ErrAlreadyExists):
		return http.StatusConflict, "conflict", "operation conflict"
	default:
		if code != "" {
			return http.StatusInternalServerError, code, "internal error"
		}
		return http.StatusInternalServerError, "internal", "internal error"
	}
}

func writeError(w http.ResponseWriter, status int, operationID, code, message string) {
	writeJSON(w, status, operationID, ErrorResponse{
		OperationID: operationID,
		Code:        code,
		Message:     message,
	})
}

func writeJSON(w http.ResponseWriter, status int, operationID string, response any) {
	header := w.Header()
	header.Set("Content-Type", "application/json")
	if operationID != "" {
		header.Set("X-Chamber-Operation-ID", operationID)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
