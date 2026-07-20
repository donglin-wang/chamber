package runtime

import (
	"context"
	"fmt"
	goruntime "runtime"
	"sort"
	"strings"

	chamberRunc "github.com/donglin-wang/chamber/pkg/runtime/runc"
	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

var runtimeCapabilities = map[string]chamberRuntimeShared.Capabilities{
	chamberRuntimeShared.RuntimeNameRunc: {
		Privileges: []capability.Privilege{
			capability.Rootless,
		},
		Isolation: []chamberRuntimeShared.Isolation{
			chamberRuntimeShared.ProcessIsolation,
		},
	},
}

func NewRuntime(ctx context.Context, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager) (chamberRuntimeShared.Runtime, error) {
	return newForGOOS(ctx, config, directoryManager, goruntime.GOOS)
}

func newForGOOS(ctx context.Context, config chamberRuntimeShared.Config, directoryManager localfs.DirectoryManager, goos string) (chamberRuntimeShared.Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
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
	if goos != "linux" {
		return nil, fmt.Errorf("Chamber runtime requires a Linux host or Linux VM; current GOOS is %q", goos)
	}
	if config.RuntimeRoot == "" {
		return nil, fmt.Errorf("%w: runtime root is required", chamberErrors.ErrInvalidRequest)
	}
	if config.RuntimeBinDir == "" {
		return nil, fmt.Errorf("%w: runtime bin dir is required", chamberErrors.ErrInvalidRequest)
	}
	if err := directoryManager.MkdirPrivate(config.RuntimeRoot); err != nil {
		return nil, fmt.Errorf("create runtime root: %w", err)
	}
	if err := directoryManager.MkdirPrivate(config.RuntimeBinDir); err != nil {
		return nil, fmt.Errorf("create runtime bin dir: %w", err)
	}

	switch config.Name {
	case chamberRuntimeShared.RuntimeNameRunc:
		return chamberRunc.New(ctx, config, directoryManager)
	default:
		return nil, fmt.Errorf("%w: unsupported runtime name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedNames(), ", "))
	}
}

func SupportedNames() []string {
	names := make([]string, 0, len(runtimeCapabilities))
	for name := range runtimeCapabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func IsSupportedName(name string) bool {
	_, ok := runtimeCapabilities[name]
	return ok
}

func SupportedCapabilities(name string) (chamberRuntimeShared.Capabilities, bool) {
	capabilities, ok := runtimeCapabilities[name]
	if !ok {
		return chamberRuntimeShared.Capabilities{}, false
	}
	return cloneCapabilities(capabilities), true
}

func supportsPrivilege(capabilities chamberRuntimeShared.Capabilities, privilege capability.Privilege) bool {
	for _, supported := range capabilities.Privileges {
		if supported == privilege {
			return true
		}
	}
	return false
}

func cloneCapabilities(capabilities chamberRuntimeShared.Capabilities) chamberRuntimeShared.Capabilities {
	return chamberRuntimeShared.Capabilities{
		Privileges: append([]capability.Privilege(nil), capabilities.Privileges...),
		Isolation:  append([]chamberRuntimeShared.Isolation(nil), capabilities.Isolation...),
	}
}
