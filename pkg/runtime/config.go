package runtime

import (
	"fmt"
	"path/filepath"
	"strings"

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

type Override struct {
	RuntimeRoot   *string                 `json:"runtime_root,omitempty"`
	RuntimeBinDir *string                 `json:"runtime_bin_dir,omitempty"`
	Name          *string                 `json:"name,omitempty"`
	Privilege     *capability.Privilege   `json:"privilege,omitempty"`
	Logging       chamberLogging.Override `json:"logging,omitempty"`
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
	if override.Privilege != nil {
		defaultConfig.Privilege = *override.Privilege
	}
	if defaultConfig.Name != "" && !IsSupportedName(defaultConfig.Name) {
		return Config{}, fmt.Errorf("unsupported runtime name %q (supported: %s)", defaultConfig.Name, strings.Join(SupportedNames(), ", "))
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
