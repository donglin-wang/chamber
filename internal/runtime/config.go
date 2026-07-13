package runtime

import (
	"fmt"
	"path/filepath"
)

const DefaultName = "runc"

type Config struct {
	RuntimeRoot   string
	RuntimeBinDir string
	Name          string
	Version       string
	URL           string
	SHA256        string
}

type Override struct {
	RuntimeRoot   *string
	RuntimeBinDir *string
	Name          *string
	Version       *string
	URL           *string
	SHA256        *string
}

func DefaultConfig(rootPath string) Config {
	return Config{
		RuntimeRoot:   filepath.Join(rootPath, "run", "runtime"),
		RuntimeBinDir: filepath.Join(rootPath, "bin"),
		Name:          DefaultName,
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
