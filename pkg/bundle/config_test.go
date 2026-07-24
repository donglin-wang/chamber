package bundle

import (
	"path/filepath"
	"testing"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
)

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := DefaultConfig(root)

	if cfg.Root != filepath.Join(root, "bundles") {
		t.Fatalf("Root = %q, want default bundle root", cfg.Root)
	}
	if cfg.Name != ProvisionerNameDirectory {
		t.Fatalf("Name = %q, want directory", cfg.Name)
	}
	if cfg.Privilege != capability.Rootless {
		t.Fatalf("Privilege = %q, want rootless", cfg.Privilege)
	}
}
