package machine

import (
	"errors"
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"strings"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

var (
	ErrRootRequired = errors.New("machine root is required")
	ErrNameRequired = errors.New("machine name is required")
)

type Config struct {
	Root    string
	Name    string
	Spec    Spec
	Start   bool
	Logging chamberLogging.Config
}

type Override struct {
	Root    *string                 `json:"root,omitempty"`
	Name    *string                 `json:"name,omitempty"`
	Spec    *Spec                   `json:"spec,omitempty"`
	Start   *bool                   `json:"start,omitempty"`
	Logging chamberLogging.Override `json:"logging,omitempty"`
}

func DefaultConfig(rootPath string) Config {
	return Config{
		Root:    filepath.Join(rootPath, "machines"),
		Spec:    DefaultSpec(),
		Logging: chamberLogging.Config{},
	}
}

func DefaultSpec() Spec {
	return Spec{
		OS:   "linux",
		Arch: goruntime.GOARCH,
		CPUs: 4,
	}
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.Root != nil {
		defaultConfig.Root = *override.Root
	}
	if override.Name != nil {
		defaultConfig.Name = *override.Name
	}
	if override.Spec != nil {
		defaultConfig.Spec = *override.Spec
	}
	if override.Start != nil {
		defaultConfig.Start = *override.Start
	}
	var err error
	defaultConfig.Logging, err = chamberLogging.Resolve(defaultConfig.Logging, override.Logging)
	if err != nil {
		return Config{}, fmt.Errorf("resolve machine logging: %w", err)
	}

	defaultConfig.Root = strings.TrimSpace(defaultConfig.Root)
	if defaultConfig.Root == "" {
		return Config{}, ErrRootRequired
	}
	defaultConfig.Root, err = filepath.Abs(defaultConfig.Root)
	if err != nil {
		return Config{}, fmt.Errorf("resolve machine root: %w", err)
	}
	if err := ValidateName(defaultConfig.Name); err != nil {
		return Config{}, err
	}
	defaultConfig.Spec = resolveSpec(defaultConfig.Spec)

	return defaultConfig, nil
}

func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return ErrNameRequired
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("machine name %q must not contain leading or trailing spaces", name)
	}
	for index, r := range name {
		if isLowerAlpha(r) || isDigit(r) || r == '-' {
			continue
		}
		return fmt.Errorf("machine name %q contains invalid character %q at position %d", name, r, index)
	}
	first := rune(name[0])
	last := rune(name[len(name)-1])
	if !isLowerAlpha(first) && !isDigit(first) {
		return fmt.Errorf("machine name %q must start with a lowercase letter or digit", name)
	}
	if !isLowerAlpha(last) && !isDigit(last) {
		return fmt.Errorf("machine name %q must end with a lowercase letter or digit", name)
	}
	return nil
}

func resolveSpec(spec Spec) Spec {
	if spec.OS == "" {
		spec.OS = "linux"
	}
	if spec.Arch == "" {
		spec.Arch = goruntime.GOARCH
	}
	return spec
}

func isLowerAlpha(r rune) bool {
	return r >= 'a' && r <= 'z'
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
