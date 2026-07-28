package image

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := DefaultConfig(root)

	if cfg.Root != filepath.Join(root, "images") {
		t.Fatalf("Root = %q, want default image root", cfg.Root)
	}
	if cfg.TmpRoot != hostfs.DefaultTmpRoot("images") {
		t.Fatalf("TmpRoot = %q, want default image temp root", cfg.TmpRoot)
	}
	if cfg.BuildKit.BuildctlPath != "" {
		t.Fatalf("BuildKit.BuildctlPath = %q, want caller-provided local buildctl path unset by default", cfg.BuildKit.BuildctlPath)
	}
}

func TestNormalizePlatformDefaultsToLinuxHostArchitecture(t *testing.T) {
	platform := NormalizePlatform(Platform{})
	if platform.OS != "linux" {
		t.Fatalf("OS = %q, want linux", platform.OS)
	}
	if platform.Architecture != runtime.GOARCH {
		t.Fatalf("Architecture = %q, want %q", platform.Architecture, runtime.GOARCH)
	}
	if platform.Variant != "" {
		t.Fatalf("Variant = %q, want empty", platform.Variant)
	}
}
