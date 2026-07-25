package factory

import (
	"context"
	"fmt"
	goruntime "runtime"
	"sort"
	"strings"

	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRunc "github.com/donglin-wang/chamber/pkg/runtime/internal/runc"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

var runtimeCapabilities = map[string]chamberRuntime.Capabilities{
	chamberRuntime.RuntimeNameRunc: {
		Privileges: []capability.Privilege{
			capability.Rootless,
		},
		Isolation: []chamberRuntime.Isolation{
			chamberRuntime.ProcessIsolation,
		},
	},
}

// NewRuntime validates config, checks host and implementation support, creates
// private runtime directories, installs or reuses runtime artifacts as needed,
// and returns a ready runtime. The supplied context controls construction work
// only; container lifecycle is owned by Container values returned from Run.
func NewRuntime(ctx context.Context, config chamberRuntime.Config, directoryManager localfs.DirectoryManager) (chamberRuntime.Runtime, error) {
	return newRuntimeForOS(ctx, config, directoryManager, goruntime.GOOS)
}

func newRuntimeForOS(ctx context.Context, config chamberRuntime.Config, directoryManager localfs.DirectoryManager, osName string) (chamberRuntime.Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: runtime construction canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if directoryManager == nil {
		return nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Name == "" {
		return nil, fmt.Errorf("%w: runtime name is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Privilege == "" {
		return nil, fmt.Errorf("%w: runtime privilege is required", chamberErrors.ErrInvalidRequest)
	}
	capabilities, ok := runtimeCapabilities[config.Name]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported runtime name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedNames(), ", "))
	}
	if !supportsPrivilege(capabilities, config.Privilege) {
		return nil, fmt.Errorf("%w: %s runtime does not support %q privilege", chamberErrors.ErrInvalidRequest, config.Name, config.Privilege)
	}
	if osName != "linux" {
		return nil, fmt.Errorf("%w: Chamber runtime requires a Linux host or Linux VM; current GOOS is %q", chamberErrors.ErrUnsupportedHost, osName)
	}
	if config.RuntimeRoot == "" {
		return nil, fmt.Errorf("%w: runtime root is required", chamberErrors.ErrInvalidRequest)
	}
	if config.RuntimeBinDir == "" {
		return nil, fmt.Errorf("%w: runtime bin dir is required", chamberErrors.ErrInvalidRequest)
	}
	if err := directoryManager.MkdirPrivate(config.RuntimeRoot); err != nil {
		return nil, fmt.Errorf("%w: create runtime root: %v", chamberErrors.ErrFilesystemFailed, err)
	}
	if err := directoryManager.MkdirPrivate(config.RuntimeBinDir); err != nil {
		return nil, fmt.Errorf("%w: create runtime bin dir: %v", chamberErrors.ErrFilesystemFailed, err)
	}

	switch config.Name {
	case chamberRuntime.RuntimeNameRunc:
		return chamberRunc.New(ctx, config, directoryManager)
	default:
		return nil, fmt.Errorf("%w: unsupported runtime name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedNames(), ", "))
	}
}

// SupportedNames returns the sorted list of runtime implementation names
// accepted by NewRuntime.
func SupportedNames() []string {
	names := make([]string, 0, len(runtimeCapabilities))
	for name := range runtimeCapabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsSupportedName reports whether name selects a runtime implementation known
// to this package.
func IsSupportedName(name string) bool {
	_, ok := runtimeCapabilities[name]
	return ok
}

// SupportedCapabilities returns a copy of the static capabilities for name. The
// boolean is false when name is not a supported runtime.
func SupportedCapabilities(name string) (chamberRuntime.Capabilities, bool) {
	capabilities, ok := runtimeCapabilities[name]
	if !ok {
		return chamberRuntime.Capabilities{}, false
	}
	return chamberRuntime.CloneCapabilities(capabilities), true
}

func supportsPrivilege(capabilities chamberRuntime.Capabilities, privilege capability.Privilege) bool {
	for _, supported := range capabilities.Privileges {
		if supported == privilege {
			return true
		}
	}
	return false
}
