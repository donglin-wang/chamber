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
)

type runtimeImplementation struct {
	name string
}

var runtimeImplementations = []runtimeImplementation{
	{
		name: chamberRuntime.RuntimeNameRunc,
	},
}

func TestRuntimeImplementationsListSharedConstructorNames(t *testing.T) {
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
