package imageref

import (
	"fmt"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/google/go-containerregistry/pkg/name"
)

func Canonical(raw string) (string, error) {
	ref, err := name.ParseReference(raw)
	if err != nil {
		return "", fmt.Errorf("%w: invalid image reference %q: %w", chamberErrors.ErrInvalidRequest, raw, err)
	}
	return ref.Name(), nil
}

func Validate(raw string) error {
	_, err := Canonical(raw)
	return err
}

func IsValid(raw string) bool {
	return Validate(raw) == nil
}
