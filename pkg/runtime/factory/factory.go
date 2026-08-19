package factory

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRunc "github.com/donglin-wang/chamber/pkg/runtime/internal/runc"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/hostprobe"
)

var runtimeNames = map[string]struct{}{
	chamberRuntime.RuntimeNameRunc: {},
}

var runtimeHostRules = []hostprobe.Rule{
	hostprobe.RequireLinux,
	hostprobe.RequireRootlessUser,
	hostprobe.RequireUserNamespacesEnabled,
	hostprobe.RequireAppArmorAllowsUserNamespaces,
	hostprobe.ProbeUserNamespace,
}

var checkRuntimeHostRules = requireRuntimeHostRules

var runtimeWorkspaceRequirements = hostfs.FeatureSet{
	PrivateDirs:      true,
	FileFsync:        true,
	AtomicFileRename: true,
}

// NewRuntime validates config, checks host and implementation name, creates
// private runtime directories and package workspaces, installs or reuses
// runtime artifacts as needed, and returns a ready runtime. The supplied context
// controls construction work only; container lifecycle is owned by Container
// values returned from Run.
func NewRuntime(ctx context.Context, config chamberRuntime.Config) (chamberRuntime.Runtime, error) {
	runtimeWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
		Root:         config.RuntimeRoot,
		TmpRoot:      config.RuntimeTmpRoot,
		Requirements: runtimeWorkspaceRequirements,
	})
	if err != nil {
		return nil, err
	}

	var binaryWorkspace *hostfs.Workspace
	if strings.TrimSpace(config.RuntimePath) == "" {
		binaryWorkspace, err = hostfs.NewWorkspace(hostfs.Config{
			Root:         config.RuntimeBinDir,
			TmpRoot:      config.RuntimeBinTmpRoot,
			Requirements: runtimeWorkspaceRequirements,
		})
		if err != nil {
			return nil, err
		}
	}
	return NewRuntimeWithWorkspace(ctx, config, runtimeWorkspace, binaryWorkspace)
}

// NewRuntimeWithWorkspace validates config and the supplied package
// workspaces, checks host and implementation name, creates private runtime
// directories, installs or reuses runtime artifacts as needed, and returns a
// ready runtime. The supplied context controls construction work only;
// container lifecycle is owned by Container values returned from Run.
func NewRuntimeWithWorkspace(ctx context.Context, config chamberRuntime.Config, runtimeWorkspace *hostfs.Workspace, binaryWorkspace *hostfs.Workspace) (chamberRuntime.Runtime, error) {
	return newRuntime(ctx, config, runtimeWorkspace, binaryWorkspace)
}

func newRuntime(ctx context.Context, config chamberRuntime.Config, runtimeWorkspace *hostfs.Workspace, binaryWorkspace *hostfs.Workspace) (chamberRuntime.Runtime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", chamberErrors.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: runtime construction canceled before start: %w", chamberErrors.ErrCanceled, err)
	}
	if runtimeWorkspace == nil {
		return nil, fmt.Errorf("%w: runtime workspace is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Name == "" {
		return nil, fmt.Errorf("%w: runtime name is required", chamberErrors.ErrInvalidRequest)
	}
	if !IsSupportedName(config.Name) {
		return nil, fmt.Errorf("%w: unsupported runtime name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedNames(), ", "))
	}
	if config.RuntimeRoot == "" {
		return nil, fmt.Errorf("%w: runtime root is required", chamberErrors.ErrInvalidRequest)
	}
	if config.RuntimeBinDir == "" && strings.TrimSpace(config.RuntimePath) == "" {
		return nil, fmt.Errorf("%w: runtime bin dir is required", chamberErrors.ErrInvalidRequest)
	}
	runtimeRoot, err := filepath.Abs(config.RuntimeRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve runtime root: %w", chamberErrors.ErrInvalidRequest, err)
	}
	if filepath.Clean(runtimeRoot) != filepath.Clean(runtimeWorkspace.Root()) {
		return nil, fmt.Errorf("%w: runtime root %q does not match workspace root %q", chamberErrors.ErrInvalidRequest, config.RuntimeRoot, runtimeWorkspace.Root())
	}
	if err := requireWorkspaceTmpRoot("runtime temporary root", config.RuntimeTmpRoot, runtimeWorkspace.TmpRoot()); err != nil {
		return nil, err
	}
	if err := requireWorkspaceFeatures("runtime workspace", runtimeWorkspace.Features(), hostfs.FeatureSet{
		PrivateDirs:      runtimeWorkspaceRequirements.PrivateDirs,
		FileFsync:        runtimeWorkspaceRequirements.FileFsync,
		AtomicFileRename: runtimeWorkspaceRequirements.AtomicFileRename,
	}); err != nil {
		return nil, err
	}
	config.RuntimeRoot = runtimeWorkspace.Root()
	config.RuntimeTmpRoot = runtimeWorkspace.TmpRoot()
	if strings.TrimSpace(config.RuntimePath) == "" {
		if binaryWorkspace == nil {
			return nil, fmt.Errorf("%w: runtime binary workspace is required", chamberErrors.ErrInvalidRequest)
		}
		binRoot, err := filepath.Abs(config.RuntimeBinDir)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve runtime bin dir: %w", chamberErrors.ErrInvalidRequest, err)
		}
		if filepath.Clean(binRoot) != filepath.Clean(binaryWorkspace.Root()) {
			return nil, fmt.Errorf("%w: runtime bin dir %q does not match binary workspace root %q", chamberErrors.ErrInvalidRequest, config.RuntimeBinDir, binaryWorkspace.Root())
		}
		if err := requireWorkspaceTmpRoot("runtime binary temporary root", config.RuntimeBinTmpRoot, binaryWorkspace.TmpRoot()); err != nil {
			return nil, err
		}
		if err := requireWorkspaceFeatures("runtime binary workspace", binaryWorkspace.Features(), hostfs.FeatureSet{
			PrivateDirs:      runtimeWorkspaceRequirements.PrivateDirs,
			FileFsync:        runtimeWorkspaceRequirements.FileFsync,
			AtomicFileRename: runtimeWorkspaceRequirements.AtomicFileRename,
		}); err != nil {
			return nil, err
		}
		config.RuntimeBinDir = binaryWorkspace.Root()
		config.RuntimeBinTmpRoot = binaryWorkspace.TmpRoot()
	}
	if err := checkRuntimeHostRules(ctx); err != nil {
		return nil, err
	}

	switch config.Name {
	case chamberRuntime.RuntimeNameRunc:
		return chamberRunc.New(ctx, config, runtimeWorkspace, binaryWorkspace)
	default:
		return nil, fmt.Errorf("%w: unsupported runtime name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedNames(), ", "))
	}
}

func requireRuntimeHostRules(ctx context.Context) error {
	var messages []string
	for _, rule := range runtimeHostRules {
		messages = append(messages, rule.Check(ctx)...)
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%w: runtime host probe failed: %s", chamberErrors.ErrUnsupportedHost, strings.Join(messages, "; "))
}

// SupportedNames returns the sorted list of runtime implementation names
// accepted by NewRuntime.
func SupportedNames() []string {
	names := make([]string, 0, len(runtimeNames))
	for name := range runtimeNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsSupportedName reports whether name selects a runtime implementation known
// to this package.
func IsSupportedName(name string) bool {
	_, ok := runtimeNames[name]
	return ok
}

func requireWorkspaceTmpRoot(label string, configured string, actual string) error {
	if strings.TrimSpace(configured) == "" {
		return nil
	}
	configuredAbs, err := filepath.Abs(configured)
	if err != nil {
		return fmt.Errorf("%w: resolve %s %q: %w", chamberErrors.ErrInvalidRequest, label, configured, err)
	}
	if filepath.Clean(configuredAbs) != filepath.Clean(actual) {
		return fmt.Errorf("%w: %s %q does not match workspace temporary root %q", chamberErrors.ErrInvalidRequest, label, configured, actual)
	}
	return nil
}

func requireWorkspaceFeatures(label string, observed hostfs.FeatureSet, required hostfs.FeatureSet) error {
	if required.PrivateDirs && !observed.PrivateDirs {
		return fmt.Errorf("%w: %s requires private directories", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.FileFsync && !observed.FileFsync {
		return fmt.Errorf("%w: %s requires file fsync", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.DirectoryFsync && !observed.DirectoryFsync {
		return fmt.Errorf("%w: %s requires directory fsync", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.AtomicFileRename && !observed.AtomicFileRename {
		return fmt.Errorf("%w: %s requires atomic file rename between temporary and durable roots", chamberErrors.ErrFilesystemFailed, label)
	}
	if required.AtomicDirectoryRename && !observed.AtomicDirectoryRename {
		return fmt.Errorf("%w: %s requires atomic directory rename between temporary and durable roots", chamberErrors.ErrFilesystemFailed, label)
	}
	return nil
}
