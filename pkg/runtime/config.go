package runtime

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type Config struct {
	RuntimeRoot   string
	RuntimeBinDir string
	Name          string
	Privilege     capability.Privilege
	Logging       chamberLogging.Config
}

func DefaultConfig(rootPath string) Config {
	return Config{
		RuntimeRoot:   filepath.Join(rootPath, "run", "runtime"),
		RuntimeBinDir: filepath.Join(rootPath, "bin"),
		Name:          RuntimeNameRunc,
		Privilege:     capability.Rootless,
		Logging:       chamberLogging.Config{},
	}
}
