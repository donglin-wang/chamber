package image

import (
	"fmt"
	"path/filepath"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type Config struct {
	Root    string
	Logging chamberLogging.Config
}

type Override struct {
	Root    *string                 `json:"root,omitempty"`
	Logging chamberLogging.Override `json:"logging,omitempty"`
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root:    filepath.Join(rootPath, "images"),
		Logging: chamberLogging.Config{},
	}
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.Root != nil {
		defaultConfig.Root = *override.Root
	}
	var err error
	defaultConfig.Logging, err = chamberLogging.Resolve(defaultConfig.Logging, override.Logging)
	if err != nil {
		return Config{}, fmt.Errorf("resolve image logging: %w", err)
	}

	defaultConfig.Root, err = absPath(defaultConfig.Root)
	if err != nil {
		return Config{}, fmt.Errorf("resolve image root: %w", err)
	}

	return defaultConfig, nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}
