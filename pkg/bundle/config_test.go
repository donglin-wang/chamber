package bundle

import (
	"path/filepath"
	"testing"

	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := DefaultConfig(root)

	if cfg.Root != filepath.Join(root, "bundles") {
		t.Fatalf("Root = %q, want default bundle root", cfg.Root)
	}
	if cfg.TmpRoot != hostfs.DefaultTmpRoot("bundles") {
		t.Fatalf("TmpRoot = %q, want default bundle temp root", cfg.TmpRoot)
	}
	if cfg.Name != ProvisionerNameDirectory {
		t.Fatalf("Name = %q, want directory", cfg.Name)
	}
}
