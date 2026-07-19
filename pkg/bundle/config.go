package bundle

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type Config struct {
	Root      string
	Privilege capability.Privilege
	Logging   chamberLogging.Config
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root:      filepath.Join(rootPath, "bundles"),
		Privilege: capability.Rootless,
		Logging:   chamberLogging.Config{},
	}
}
