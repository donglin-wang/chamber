package factory

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberDirectoryProvisioner "github.com/donglin-wang/chamber/pkg/bundle/internal/directory"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
)

var provisionerCapabilities = map[string]chamberBundle.Capabilities{
	chamberBundle.ProvisionerNameDirectory: {
		Privileges: []capability.Privilege{
			capability.Rootless,
		},
	},
}

// NewProvisioner validates config, creates the configured private bundle root,
// checks the selected implementation capabilities, and returns a ready bundle
// provisioner. Callers own bundle-root placement, cleanup, cancellation policy,
// and recovery.
func NewProvisioner(config chamberBundle.Config, workspace *hostfs.Workspace) (chamberBundle.Provisioner, error) {
	if workspace == nil {
		return nil, fmt.Errorf("%w: bundle workspace is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Name == "" {
		return nil, fmt.Errorf("%w: bundle provisioner name is required", chamberErrors.ErrInvalidRequest)
	}
	if config.Privilege == "" {
		return nil, fmt.Errorf("%w: bundle privilege is required", chamberErrors.ErrInvalidRequest)
	}
	capabilities, ok := provisionerCapabilities[config.Name]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported bundle provisioner name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedProvisionerNames(), ", "))
	}
	if !supportsPrivilege(capabilities, config.Privilege) {
		return nil, fmt.Errorf("%w: %s bundle provisioner does not support %q privilege", chamberErrors.ErrInvalidRequest, config.Name, config.Privilege)
	}
	if config.Root == "" {
		return nil, fmt.Errorf("%w: bundle root is required", chamberErrors.ErrInvalidRequest)
	}
	configRoot, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve bundle root: %w", chamberErrors.ErrInvalidRequest, err)
	}
	if filepath.Clean(configRoot) != filepath.Clean(workspace.Root()) {
		return nil, fmt.Errorf("%w: bundle root %q does not match workspace root %q", chamberErrors.ErrInvalidRequest, config.Root, workspace.Root())
	}
	if err := requireWorkspaceTmpRoot("bundle temporary root", config.TmpRoot, workspace.TmpRoot()); err != nil {
		return nil, err
	}
	if err := requireWorkspaceCapabilities("bundle workspace", workspace.Capabilities(), hostfs.Capabilities{
		PrivateDirs:           true,
		AtomicDirectoryRename: true,
	}); err != nil {
		return nil, err
	}
	config.Root = workspace.Root()
	config.TmpRoot = workspace.TmpRoot()

	switch config.Name {
	case chamberBundle.ProvisionerNameDirectory:
		return chamberDirectoryProvisioner.New(config, workspace)
	default:
		return nil, fmt.Errorf("%w: unsupported bundle provisioner name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedProvisionerNames(), ", "))
	}
}

// SupportedProvisionerNames returns the sorted list of provisioner
// implementation names accepted by NewProvisioner.
func SupportedProvisionerNames() []string {
	names := make([]string, 0, len(provisionerCapabilities))
	for name := range provisionerCapabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsSupportedProvisionerName reports whether name selects a provisioner
// implementation known to this package.
func IsSupportedProvisionerName(name string) bool {
	_, ok := provisionerCapabilities[name]
	return ok
}

// SupportedProvisionerCapabilities returns a copy of the static capabilities
// for name. The boolean is false when name is not a supported provisioner.
func SupportedProvisionerCapabilities(name string) (chamberBundle.Capabilities, bool) {
	capabilities, ok := provisionerCapabilities[name]
	if !ok {
		return chamberBundle.Capabilities{}, false
	}
	return chamberBundle.CloneCapabilities(capabilities), true
}

func supportsPrivilege(capabilities chamberBundle.Capabilities, privilege capability.Privilege) bool {
	for _, supported := range capabilities.Privileges {
		if supported == privilege {
			return true
		}
	}
	return false
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

func requireWorkspaceCapabilities(label string, observed hostfs.Capabilities, required hostfs.Capabilities) error {
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
