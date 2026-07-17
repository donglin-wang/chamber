package containerid

import (
	"errors"
	"strings"
	"testing"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestValidateAcceptsSafeIDs(t *testing.T) {
	tests := []string{
		"container-1",
		"container_1",
		"container.1",
		"C1",
		strings.Repeat("a", 128),
	}
	for _, test := range tests {
		t.Run(test, func(t *testing.T) {
			if err := Validate(test); err != nil {
				t.Fatalf("Validate(%q) error = %v", test, err)
			}
			if !IsValid(test) {
				t.Fatalf("IsValid(%q) = false, want true", test)
			}
		})
	}
}

func TestValidateRejectsUnsafeIDs(t *testing.T) {
	tests := []string{
		"",
		".",
		"..",
		".starts-with-dot",
		"-starts-with-dash",
		"../escape",
		"/absolute",
		"has/slash",
		"has space",
		strings.Repeat("a", 129),
	}
	for _, test := range tests {
		t.Run(test, func(t *testing.T) {
			err := Validate(test)
			if err == nil {
				t.Fatalf("Validate(%q) error = nil, want error", test)
			}
			if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
				t.Fatalf("Validate(%q) error = %v, want invalid request code", test, err)
			}
			if IsValid(test) {
				t.Fatalf("IsValid(%q) = true, want false", test)
			}
		})
	}
}
