package runtime

import (
	"fmt"
	"path/filepath"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

const DefaultName = "runc"

type Config struct {
	RuntimeRoot   string
	RuntimeBinDir string
	Name          string
	Version       string
	URL           string
	SHA256        string
	Logging       chamberLogging.Config
}

type Override struct {
	RuntimeRoot   *string                 `json:"runtime_root,omitempty"`
	RuntimeBinDir *string                 `json:"runtime_bin_dir,omitempty"`
	Name          *string                 `json:"name,omitempty"`
	Version       *string                 `json:"version,omitempty"`
	URL           *string                 `json:"url,omitempty"`
	SHA256        *string                 `json:"sha256,omitempty"`
	Logging       chamberLogging.Override `json:"logging,omitempty"`
}

func DefaultConfig(rootPath string) Config {
	return Config{
		RuntimeRoot:   filepath.Join(rootPath, "run", "runtime"),
		RuntimeBinDir: filepath.Join(rootPath, "bin"),
		Name:          DefaultName,
		Logging:       chamberLogging.Config{},
	}
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.RuntimeRoot != nil {
		defaultConfig.RuntimeRoot = *override.RuntimeRoot
	}
	if override.RuntimeBinDir != nil {
		defaultConfig.RuntimeBinDir = *override.RuntimeBinDir
	}
	if override.Name != nil {
		defaultConfig.Name = *override.Name
	}
	if override.Version != nil {
		defaultConfig.Version = *override.Version
	}
	if override.URL != nil {
		defaultConfig.URL = *override.URL
	}
	if override.SHA256 != nil {
		defaultConfig.SHA256 = *override.SHA256
	}
	var err error
	defaultConfig.Logging, err = chamberLogging.Resolve(defaultConfig.Logging, override.Logging)
	if err != nil {
		return Config{}, fmt.Errorf("resolve runtime logging: %w", err)
	}

	defaultConfig.RuntimeRoot, err = absPath(defaultConfig.RuntimeRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve runtime root: %w", err)
	}
	defaultConfig.RuntimeBinDir, err = absPath(defaultConfig.RuntimeBinDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve runtime bin dir: %w", err)
	}

	return defaultConfig, nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}
