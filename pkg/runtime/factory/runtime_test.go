package factory

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
)

type runtimeImplementation struct {
	name       string
	privilege  capability.Privilege
	privileges []capability.Privilege
	isolation  []chamberRuntime.Isolation
}

var runtimeImplementations = []runtimeImplementation{
	{
		name:      chamberRuntime.RuntimeNameRunc,
		privilege: capability.Rootless,
		privileges: []capability.Privilege{
			capability.Rootless,
		},
		isolation: []chamberRuntime.Isolation{
			chamberRuntime.ProcessIsolation,
		},
	},
}

func TestRuntimeImplementationsListSharedConstructorCapabilities(t *testing.T) {
	wantNames := make([]string, 0, len(runtimeImplementations))
	for _, implementation := range runtimeImplementations {
		wantNames = append(wantNames, implementation.name)
		if !IsSupportedName(implementation.name) {
			t.Fatalf("IsSupportedName(%q) = false, want true", implementation.name)
		}
	}
	sort.Strings(wantNames)
	if !slices.Equal(SupportedNames(), wantNames) {
		t.Fatalf("SupportedNames() = %#v, want %#v", SupportedNames(), wantNames)
	}

	for _, implementation := range runtimeImplementations {
		t.Run(implementation.name, func(t *testing.T) {
			capabilities, ok := SupportedCapabilities(implementation.name)
			if !ok {
				t.Fatalf("SupportedCapabilities(%q) ok = false, want true", implementation.name)
			}
			if !slices.Equal(capabilities.Privileges, implementation.privileges) {
				t.Fatalf("privileges = %#v, want %#v", capabilities.Privileges, implementation.privileges)
			}
			if !slices.Equal(capabilities.Isolation, implementation.isolation) {
				t.Fatalf("isolation = %#v, want %#v", capabilities.Isolation, implementation.isolation)
			}
		})
	}
}

func TestRuntimeImplementationsRejectUnsupportedPrivilege(t *testing.T) {
	for _, implementation := range runtimeImplementations {
		t.Run(implementation.name, func(t *testing.T) {
			runtimeRoot := filepath.Join(t.TempDir(), "runtime")
			binDir := filepath.Join(t.TempDir(), "bin")

			config := chamberRuntime.Config{
				RuntimeRoot:   runtimeRoot,
				RuntimeBinDir: binDir,
				Name:          implementation.name,
				Privilege:     capability.Rootful,
			}
			_, err := NewRuntime(context.Background(), config, newTestWorkspace(t, runtimeRoot), newTestWorkspace(t, binDir))

			if err == nil {
				t.Fatal("NewRuntime() error = nil, want unsupported privilege error")
			}
			if !strings.Contains(err.Error(), "does not support") {
				t.Fatalf("NewRuntime() error = %v, want unsupported privilege", err)
			}
		})
	}
}

func TestRuntimeImplementationsRejectNonLinuxHost(t *testing.T) {
	if goruntime.GOOS == "linux" {
		t.Skip("Linux hosts pass the shared OS gate")
	}
	for _, implementation := range runtimeImplementations {
		t.Run(implementation.name, func(t *testing.T) {
			runtimeRoot := filepath.Join(t.TempDir(), "runtime")
			binDir := filepath.Join(t.TempDir(), "bin")

			config := chamberRuntime.Config{
				RuntimeRoot:   runtimeRoot,
				RuntimeBinDir: binDir,
				Name:          implementation.name,
				Privilege:     implementation.privilege,
			}
			_, err := NewRuntime(context.Background(), config, newTestWorkspace(t, runtimeRoot), newTestWorkspace(t, binDir))

			if err == nil {
				t.Fatal("NewRuntime() error = nil, want Linux host requirement")
			}
			if !strings.Contains(err.Error(), "requires a Linux host or Linux VM") {
				t.Fatalf("NewRuntime() error = %v, want Linux host explanation", err)
			}
		})
	}
}
