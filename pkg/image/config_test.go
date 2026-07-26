package image

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	root := t.TempDir()

	cfg := DefaultConfig(root)

	if cfg.Root != filepath.Join(root, "images") {
		t.Fatalf("Root = %q, want default image root", cfg.Root)
	}
	if cfg.Buildah.Path != "" {
		t.Fatalf("Buildah.Path = %q, want caller-provided local worker path unset by default", cfg.Buildah.Path)
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
