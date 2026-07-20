package shared

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

const ProvisionerNameDirectory = "directory"

type Config struct {
	Root      string
	Name      string
	Privilege capability.Privilege
	Logging   chamberLogging.Config
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root:      filepath.Join(rootPath, "bundles"),
		Name:      ProvisionerNameDirectory,
		Privilege: capability.Rootless,
		Logging:   chamberLogging.Config{},
	}
}
