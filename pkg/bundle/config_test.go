package bundle

import (
	"path/filepath"
	"testing"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
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

	if cfg.Root != filepath.Join(root, "bundles") {
		t.Fatalf("Root = %q, want default bundle root", cfg.Root)
	}
	if cfg.Privilege != capability.Rootless {
		t.Fatalf("Privilege = %q, want rootless", cfg.Privilege)
	}
	if cfg.Logging != (chamberLogging.Config{Level: "debug", Format: "text"}) {
		t.Fatalf("Logging = %#v, want debug text", cfg.Logging)
	}
}

func TestResolveAppliesPrivilegeOverride(t *testing.T) {
	rootful := capability.Rootful

	cfg, err := Resolve(DefaultConfig(t.TempDir()), Override{
		Privilege: &rootful,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if cfg.Privilege != capability.Rootful {
		t.Fatalf("Privilege = %q, want rootful", cfg.Privilege)
	}
}

func ptr(value string) *string {
	return &value
}
