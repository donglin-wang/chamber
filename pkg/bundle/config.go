package bundle

import (
	"fmt"
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

type Config struct {
	Root      string
	Privilege capability.Privilege
	Logging   chamberLogging.Config
}

type Override struct {
	Root      *string                 `json:"root,omitempty"`
	Privilege *capability.Privilege   `json:"privilege,omitempty"`
	Logging   chamberLogging.Override `json:"logging,omitempty"`
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root:      filepath.Join(rootPath, "bundles"),
		Privilege: capability.Rootless,
		Logging:   chamberLogging.Config{},
	}
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.Root != nil {
		defaultConfig.Root = *override.Root
	}
	if override.Privilege != nil {
		defaultConfig.Privilege = *override.Privilege
	}
	var err error
	defaultConfig.Logging, err = chamberLogging.Resolve(defaultConfig.Logging, override.Logging)
	if err != nil {
		return Config{}, fmt.Errorf("resolve bundle logging: %w", err)
	}

	defaultConfig.Root, err = absPath(defaultConfig.Root)
	if err != nil {
		return Config{}, fmt.Errorf("resolve bundle root: %w", err)
	}

	return defaultConfig, nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}
