package image_test

import (
	"errors"
	"testing"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestCanonicalImageReferenceNormalizesDockerHubRegistry(t *testing.T) {
	canonical, err := chamberImage.CanonicalImageReference("docker.io/library/golang:1.26.4-bookworm")
	if err != nil {
		t.Fatalf("CanonicalImageReference() error = %v", err)
	}
	if canonical != "index.docker.io/library/golang:1.26.4-bookworm" {
		t.Fatalf("CanonicalImageReference() = %q, want index.docker.io reference", canonical)
	}
}

func TestValidateImageReferenceRejectsInvalidReferences(t *testing.T) {
	err := chamberImage.ValidateImageReference("not a reference !!")
	if err == nil {
		t.Fatal("ValidateImageReference() error = nil, want invalid reference error")
	}
	if !errors.Is(err, chamberErrors.ErrInvalidImageReference) {
		t.Fatalf("ValidateImageReference() error = %v, want invalid image reference code", err)
	}
	if chamberImage.IsValidImageReference("not a reference !!") {
		t.Fatal("IsValidImageReference() = true, want false")
	}
}
