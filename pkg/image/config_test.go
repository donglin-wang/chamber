package image

import (
	"errors"
	"path/filepath"
	"testing"

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := DefaultConfig(root)

	if cfg.Root != filepath.Join(root, "images") {
		t.Fatalf("Root = %q, want default image root", cfg.Root)
	}
}

func TestDestinationForCanonicalImageIncludesPlatform(t *testing.T) {
	root := t.TempDir()
	amd64Path, err := DestinationForCanonicalImage(root, "example.com/library/app:latest", Platform{
		OS:           "linux",
		Architecture: "amd64",
	})
	if err != nil {
		t.Fatalf("DestinationForCanonicalImage(amd64) error = %v", err)
	}
	arm64Path, err := DestinationForCanonicalImage(root, "example.com/library/app:latest", Platform{
		OS:           "linux",
		Architecture: "arm64",
	})
	if err != nil {
		t.Fatalf("DestinationForCanonicalImage(arm64) error = %v", err)
	}
	if amd64Path == arm64Path {
		t.Fatalf("platform-specific destinations matched: %q", amd64Path)
	}
}

func TestDestinationForCanonicalImageWrapsInvalidRequest(t *testing.T) {
	_, err := DestinationForCanonicalImage("", "example.com/library/app:latest", Platform{})
	if !errors.Is(err, chamberErrors.ErrInvalidRequest) {
		t.Fatalf("DestinationForCanonicalImage() error = %v, want invalid request code", err)
	}
}
