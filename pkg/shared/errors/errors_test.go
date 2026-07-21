package errors

import (
	"errors"
	"fmt"
	"testing"
)

func TestCodeIsStringAndError(t *testing.T) {
	code := ErrRuntimeStartFailed

	if string(code) != "runtime_start_failed" {
		t.Fatalf("string(code) = %q, want runtime_start_failed", string(code))
	}
	if code.Error() != "runtime_start_failed" {
		t.Fatalf("code.Error() = %q, want runtime_start_failed", code.Error())
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", code), ErrRuntimeStartFailed) {
		t.Fatal("errors.Is(wrapped, ErrRuntimeStartFailed) = false, want true")
	}
}

func TestRuntimeSDKErrorCodes(t *testing.T) {
	tests := map[Code]string{
		ErrInvalidContainerID:    "invalid_container_id",
		ErrInvalidImageReference: "invalid_image_reference",
		ErrInvalidImageLayout:    "invalid_image_layout",
		ErrInvalidBundleMount:    "invalid_bundle_mount",
		ErrInvalidProcessSpec:    "invalid_process_spec",
		ErrCanceled:              "canceled",
		ErrUnsupportedHost:       "unsupported_host",
		ErrFilesystemFailed:      "filesystem_failed",
		ErrRuntimeInstallFailed:  "runtime_install_failed",
		ErrRuntimeControlFailed:  "runtime_control_failed",
	}

	for code, want := range tests {
		t.Run(want, func(t *testing.T) {
			if string(code) != want {
				t.Fatalf("string(code) = %q, want %q", string(code), want)
			}
			if !errors.Is(fmt.Errorf("wrapped: %w", code), code) {
				t.Fatalf("errors.Is(wrapped, %s) = false, want true", code)
			}
		})
	}
}
