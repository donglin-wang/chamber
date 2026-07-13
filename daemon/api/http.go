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

type ErrorResponse struct {
	OperationID string `json:"operation_id,omitempty"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

type Service interface {
	Pull(ctx context.Context, request daemon.PullRequest) (daemon.PullResult, error)
	Run(ctx context.Context, request daemon.RunRequest) (daemon.RunResult, error)
}

func NewHandler(service Service) http.Handler {
	return &handler{service: service}
}

type handler struct {
	service Service
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
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
	default:
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
		slog.Default().Error("daemon request failed", "error", err, "operation_id", operationID)
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
	case code == string(metadata.ErrStateConflict),
		errors.Is(err, metadata.ErrStateConflict),
		errors.Is(err, metadata.ErrAlreadyExists):
		return http.StatusConflict, "conflict", "operation conflict"
	default:
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
