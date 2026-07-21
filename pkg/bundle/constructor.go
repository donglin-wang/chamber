package bundle

import (
	"fmt"
	"sort"
	"strings"

	chamberDirectoryProvisioner "github.com/donglin-wang/chamber/pkg/bundle/directory"
	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

var provisionerCapabilities = map[string]chamberBundleShared.Capabilities{
	chamberBundleShared.ProvisionerNameDirectory: {
		Privileges: []capability.Privilege{
			capability.Rootless,
		},
	},
}

func NewProvisioner(config chamberBundleShared.Config, directoryManager localfs.DirectoryManager) (chamberBundleShared.Provisioner, error) {
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
	case chamberBundleShared.ProvisionerNameDirectory:
		return chamberDirectoryProvisioner.New(config, directoryManager)
	default:
		return nil, fmt.Errorf("%w: unsupported bundle provisioner name %q (supported: %s)", chamberErrors.ErrInvalidRequest, config.Name, strings.Join(SupportedProvisionerNames(), ", "))
	}
}

func SupportedProvisionerNames() []string {
	names := make([]string, 0, len(provisionerCapabilities))
	for name := range provisionerCapabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func IsSupportedProvisionerName(name string) bool {
	_, ok := provisionerCapabilities[name]
	return ok
}

func SupportedProvisionerCapabilities(name string) (chamberBundleShared.Capabilities, bool) {
	capabilities, ok := provisionerCapabilities[name]
	if !ok {
		return chamberBundleShared.Capabilities{}, false
	}
	return chamberBundleShared.CloneCapabilities(capabilities), true
}

func supportsPrivilege(capabilities chamberBundleShared.Capabilities, privilege capability.Privilege) bool {
	for _, supported := range capabilities.Privileges {
		if supported == privilege {
			return true
		}
	}
	return false
}
