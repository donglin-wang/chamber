package bundle

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

// ProvisionerNameDirectory selects Chamber's directory-backed OCI bundle
// provisioner.
const ProvisionerNameDirectory = "directory"

// Config is the final caller-provided configuration for bundle provisioning.
type Config struct {
	// Root is the private directory where provisioned bundle directories are
	// staged and published.
	Root string

	// Name selects the bundle provisioner implementation.
	Name string

	// Privilege selects the host privilege mode the provisioner must support.
	Privilege capability.Privilege

	// Logging configures host-side Chamber logs for bundle operations. A zero
	// value inherits the package logger.
	Logging chamberLogging.Config
}

// DefaultConfig returns rootless directory-provisioner configuration rooted
// below rootPath.
func DefaultConfig(rootPath string) Config {
	return Config{
		Root:      filepath.Join(rootPath, "bundles"),
		Name:      ProvisionerNameDirectory,
		Privilege: capability.Rootless,
		Logging:   chamberLogging.Config{},
	}
}
