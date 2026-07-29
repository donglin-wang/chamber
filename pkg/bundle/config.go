package bundle

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
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

	// TmpRoot is the private directory where the provisioner keeps temporary
	// bundle directories. Empty uses a user-scoped directory below the host
	// temporary directory.
	TmpRoot string

	// Name selects the bundle provisioner implementation.
	Name string

	// Logging configures host-side Chamber logs for bundle operations. A zero
	// value inherits the package logger.
	Logging chamberLogging.Config
}

// DefaultConfig returns rootless directory-provisioner configuration rooted
// below rootPath.
func DefaultConfig(rootPath string) Config {
	return Config{
		Root:      filepath.Join(rootPath, "bundles"),
		TmpRoot:   hostfs.DefaultTmpRoot("bundles"),
		Name:      ProvisionerNameDirectory,
		Logging:   chamberLogging.Config{},
	}
}
