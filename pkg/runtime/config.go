package runtime

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

// RuntimeNameRunc selects Chamber's runc-backed runtime implementation.
const RuntimeNameRunc = "runc"

// Config is the final caller-provided configuration for runtime execution.
type Config struct {
	// RuntimeRoot is the private directory where runtime state and default logs
	// are stored.
	RuntimeRoot string

	// RuntimeBinDir is the private directory where runtime binaries are stored
	// or discovered.
	RuntimeBinDir string

	// Name selects the runtime implementation.
	Name string

	// Privilege selects the host privilege mode the runtime must support.
	Privilege capability.Privilege

	// Logging configures host-side Chamber logs for runtime operations. A zero
	// value inherits the package logger.
	Logging chamberLogging.Config
}

// DefaultConfig returns rootless runc configuration rooted below rootPath.
func DefaultConfig(rootPath string) Config {
	return Config{
		RuntimeRoot:   filepath.Join(rootPath, "run", "runtime"),
		RuntimeBinDir: filepath.Join(rootPath, "bin"),
		Name:          RuntimeNameRunc,
		Privilege:     capability.Rootless,
		Logging:       chamberLogging.Config{},
	}
}
