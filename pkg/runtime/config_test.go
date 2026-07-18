package runtime

import (
	"path/filepath"
	"strings"
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

	if cfg.RuntimeRoot != filepath.Join(root, "run", "runtime") {
		t.Fatalf("RuntimeRoot = %q, want default runtime root", cfg.RuntimeRoot)
	}
	if cfg.Name != RuntimeNameRunc {
		t.Fatalf("Name = %q, want runc", cfg.Name)
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

func TestResolveRejectsUnsupportedRuntimeName(t *testing.T) {
	_, err := Resolve(DefaultConfig(t.TempDir()), Override{
		Name: ptr("crun"),
	})
	if err == nil {
		t.Fatal("Resolve() error = nil, want unsupported runtime name error")
	}
	if !strings.Contains(err.Error(), "unsupported runtime name") {
		t.Fatalf("Resolve() error = %v, want unsupported runtime name", err)
	}
}

func ptr(value string) *string {
	return &value
}
