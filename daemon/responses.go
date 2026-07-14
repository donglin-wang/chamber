package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/donglin-wang/chamber/daemon/metadata"
)

const maxRequestBodyBytes int64 = 1 << 20

type errorResponse struct {
	OperationID string `json:"operation_id,omitempty"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

type daemonError struct {
	OperationID string
	Code        metadata.ErrorCode
	Err         error
}

func (e *daemonError) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return e.Err.Error()
}

func (e *daemonError) Unwrap() error { return e.Err }

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeOperationError(w, status, "", code, message)
}

func writeOperationError(w http.ResponseWriter, status int, operationID string, code string, message string) {
	writeJSON(w, status, errorResponse{
		OperationID: operationID,
		Code:        code,
		Message:     message,
	})
}

func writeOperationJSON(w http.ResponseWriter, status int, operationID string, value any) {
	if operationID != "" {
		w.Header().Set("X-Chamber-Operation-ID", operationID)
	}
	writeJSON(w, status, value)
}

func writeDaemonError(w http.ResponseWriter, err error) {
	var daemonErr *daemonError
	operationID := ""
	if errors.As(err, &daemonErr) {
		operationID = daemonErr.OperationID
	}

	status, code, message := publicError(err, daemonErr)
	if status == http.StatusInternalServerError {
		slog.Default().Error("daemon request failed", "operation_id", operationID, "code", code, "error", err)
	}
	writeOperationError(w, status, operationID, code, message)
}

func publicError(err error, daemonErr *daemonError) (int, string, string) {
	code := metadata.ErrorCode("")
	if daemonErr != nil {
		code = daemonErr.Code
	}

	switch {
	case code == metadata.ErrInvalidRequest, errors.Is(err, metadata.ErrInvalidRequest):
		return http.StatusBadRequest, string(metadata.ErrInvalidRequest), "invalid request"
	case code == metadata.ErrImageNotFound, errors.Is(err, metadata.ErrImageNotFound):
		return http.StatusNotFound, string(metadata.ErrImageNotFound), "image not found"
	case code == metadata.ErrContainerNotFound, errors.Is(err, metadata.ErrContainerNotFound):
		return http.StatusNotFound, string(metadata.ErrContainerNotFound), "container not found"
	case code == metadata.ErrLogNotFound, errors.Is(err, metadata.ErrLogNotFound):
		return http.StatusNotFound, string(metadata.ErrLogNotFound), "container log not found"
	case code == metadata.ErrStateConflict,
		errors.Is(err, metadata.ErrStateConflict),
		errors.Is(err, metadata.ErrAlreadyExists):
		return http.StatusConflict, "conflict", "operation conflict"
	default:
		if code != "" {
			return http.StatusInternalServerError, string(code), "internal error"
		}
		return http.StatusInternalServerError, "internal", "internal error"
	}
}

func operationError(operationID string, code metadata.ErrorCode, err error) error {
	return &daemonError{
		OperationID: operationID,
		Code:        code,
		Err:         err,
	}
}
