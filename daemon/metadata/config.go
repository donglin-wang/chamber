package metadata

import (
	"fmt"
	"path/filepath"
)

type Config struct {
	Root string
}

type Override struct {
	Root *string `json:"root,omitempty"`
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root: filepath.Join(rootPath, "metadata"),
	}
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.Root != nil {
		defaultConfig.Root = *override.Root
	}

	var err error
	defaultConfig.Root, err = absPath(defaultConfig.Root)
	if err != nil {
		return Config{}, fmt.Errorf("resolve metadata root: %w", err)
	}

	return defaultConfig, nil
}

func absPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Abs(path)
}
