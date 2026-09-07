package bundle

import (
	"errors"
	"strings"
	"testing"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestValidateBundleID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"bundle-1", "bundle_1", "bundle.1", strings.Repeat("a", 128)} {
		id := id
		t.Run("valid_"+id[:min(len(id), 16)], func(t *testing.T) {
			t.Parallel()
			if err := ValidateBundleID(id); err != nil {
				t.Fatalf("ValidateBundleID(%q) error = %v", id, err)
			}
		})
	}

	for _, id := range []string{"", ".", "..", "-bundle", ".bundle", "bundle/path", strings.Repeat("a", 129)} {
		id := id
		t.Run("invalid_"+strings.ReplaceAll(id, "/", "_"), func(t *testing.T) {
			t.Parallel()
			err := ValidateBundleID(id)
			if !errors.Is(err, chamberErrors.ErrInvalidBundleID) {
				t.Fatalf("ValidateBundleID(%q) error = %v, want %v", id, err, chamberErrors.ErrInvalidBundleID)
			}
		})
	}
}
