package imageref

import (
	"errors"
	"testing"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestCanonicalNormalizesDockerHubRegistry(t *testing.T) {
	canonical, err := Canonical("docker.io/library/golang:1.26.4-bookworm")
	if err != nil {
		t.Fatalf("Canonical() error = %v", err)
	}
	if canonical != "index.docker.io/library/golang:1.26.4-bookworm" {
		t.Fatalf("Canonical() = %q, want index.docker.io reference", canonical)
	}
}

func TestValidateRejectsInvalidReferences(t *testing.T) {
	err := Validate("not a reference !!")
	if err == nil {
		t.Fatal("Validate() error = nil, want invalid reference error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("Validate() error = %v, want invalid request code", err)
	}
	if IsValid("not a reference !!") {
		t.Fatal("IsValid() = true, want false")
	}
}
