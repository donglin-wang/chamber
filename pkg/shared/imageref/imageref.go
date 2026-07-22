package imageref

import (
	"fmt"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/google/go-containerregistry/pkg/name"
)

// Canonical parses raw as an OCI image reference and returns its canonical
// string form.
func Canonical(raw string) (string, error) {
	ref, err := name.ParseReference(raw)
	if err != nil {
		return "", fmt.Errorf("%w: invalid image reference %q: %w", chamberErrors.ErrInvalidImageReference, raw, err)
	}
	return ref.Name(), nil
}

// Validate checks that raw is an acceptable OCI image reference.
func Validate(raw string) error {
	_, err := Canonical(raw)
	return err
}

// IsValid reports whether raw is an acceptable OCI image reference.
func IsValid(raw string) bool {
	return Validate(raw) == nil
}
