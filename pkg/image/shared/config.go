package shared

import (
	"path/filepath"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

// Config is the final caller-provided configuration for image operations.
type Config struct {
	// Root is the private directory where pulled OCI image layouts are stored.
	Root string

	// Logging configures host-side Chamber logs for image operations. A zero
	// value inherits the package logger.
	Logging chamberLogging.Config
}

// DefaultConfig returns image configuration rooted below rootPath.
func DefaultConfig(rootPath string) Config {
	return Config{
		Root:    filepath.Join(rootPath, "images"),
		Logging: chamberLogging.Config{},
	}
}
