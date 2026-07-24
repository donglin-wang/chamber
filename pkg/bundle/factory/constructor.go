package factory

import (
	"fmt"
	"sort"
	"strings"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberDirectoryProvisioner "github.com/donglin-wang/chamber/pkg/bundle/internal/directory"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
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
func NewProvisioner(config chamberBundle.Config, directoryManager localfs.DirectoryManager) (chamberBundle.Provisioner, error) {
	if directoryManager == nil {
		return nil, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
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
	if err := directoryManager.MkdirPrivate(config.Root); err != nil {
		return nil, fmt.Errorf("%w: create bundle root: %v", chamberErrors.ErrFilesystemFailed, err)
	}

	switch config.Name {
	case chamberBundle.ProvisionerNameDirectory:
		return chamberDirectoryProvisioner.New(config, directoryManager)
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
