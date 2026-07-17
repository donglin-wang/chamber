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
