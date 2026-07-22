package containerid

import (
	"fmt"
	"regexp"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Validate checks that id is accepted by Chamber bundle and runtime operations.
func Validate(id string) error {
	if !validID.MatchString(id) || id == "." || id == ".." {
		return fmt.Errorf("%w: invalid container ID %q", chamberErrors.ErrInvalidContainerID, id)
	}
	return nil
}

// IsValid reports whether id is accepted by Chamber bundle and runtime
// operations.
func IsValid(id string) bool {
	return Validate(id) == nil
}
