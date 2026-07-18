package image

import (
	"path/filepath"
	"testing"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

func TestResolveAppliesLoggingOverride(t *testing.T) {
	root := t.TempDir()

	cfg, err := Resolve(DefaultConfig(root), Override{
		Logging: chamberLogging.Override{
			Level:  ptr("debug"),
			Format: ptr("text"),
		},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if cfg.Root != filepath.Join(root, "images") {
		t.Fatalf("Root = %q, want default image root", cfg.Root)
	}
	if cfg.Logging != (chamberLogging.Config{Level: "debug", Format: "text"}) {
		t.Fatalf("Logging = %#v, want debug text", cfg.Logging)
	}
}

func ptr(value string) *string {
	return &value
}
