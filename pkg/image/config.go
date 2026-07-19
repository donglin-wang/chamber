package image

import (
	"path/filepath"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type Config struct {
	Root    string
	Logging chamberLogging.Config
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root:    filepath.Join(rootPath, "images"),
		Logging: chamberLogging.Config{},
	}
}
