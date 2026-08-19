package factory

import (
	"slices"
	"sort"
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
