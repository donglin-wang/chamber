package main

import (
	"fmt"
	"net/http"
	"testing"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestPublicErrorMapsSharedCodes(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		daemonErr *daemonError
		status    int
		code      string
		message   string
	}{
		{
			name:    "wrapped invalid request",
			err:     fmt.Errorf("decode: %w", chamberErrors.ErrInvalidRequest),
			status:  http.StatusBadRequest,
			code:    string(chamberErrors.ErrInvalidRequest),
			message: "invalid request",
		},
		{
			name:    "wrapped image missing",
			err:     fmt.Errorf("lookup: %w", chamberErrors.ErrImageNotFound),
			status:  http.StatusNotFound,
			code:    string(chamberErrors.ErrImageNotFound),
			message: "image not found",
		},
		{
			name: "daemon code wins",
			err:  fmt.Errorf("pull failed"),
			daemonErr: &daemonError{
				Code: chamberErrors.ErrPullFailed,
			},
			status:  http.StatusInternalServerError,
			code:    string(chamberErrors.ErrPullFailed),
			message: "internal error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, code, message := publicError(test.err, test.daemonErr)
			if status != test.status || code != test.code || message != test.message {
				t.Fatalf("publicError() = (%d, %q, %q), want (%d, %q, %q)", status, code, message, test.status, test.code, test.message)
			}
		})
	}
}
