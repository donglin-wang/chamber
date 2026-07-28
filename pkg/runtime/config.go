package runtime

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

// RuntimeNameRunc selects Chamber's runc-backed runtime implementation.
const RuntimeNameRunc = "runc"

// Config is the final caller-provided configuration for runtime execution.
type Config struct {
	// RuntimeRoot is the private directory where runtime state and default logs
	// are stored.
	RuntimeRoot string

	// RuntimeTmpRoot is the private directory where runtime state construction
	// keeps temporary files. Empty uses a user-scoped directory below the host
	// temporary directory.
	RuntimeTmpRoot string

	// RuntimeBinDir is the private directory where runtime binaries are stored
	// or discovered.
	RuntimeBinDir string

	// RuntimeBinTmpRoot is the private directory where managed runtime binary
	// installation keeps temporary files. Empty uses a user-scoped directory
	// below the host temporary directory.
	RuntimeBinTmpRoot string

	// RuntimePath is an absolute path to an existing runtime binary. Empty lets
	// Chamber download and cache the managed runtime below RuntimeBinDir.
	RuntimePath string

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
		RuntimeRoot:       filepath.Join(rootPath, "run", "runtime"),
		RuntimeTmpRoot:    hostfs.DefaultTmpRoot("runtime"),
		RuntimeBinDir:     filepath.Join(rootPath, "bin"),
		RuntimeBinTmpRoot: hostfs.DefaultTmpRoot("runtime-bin"),
		Name:              RuntimeNameRunc,
		Privilege:         capability.Rootless,
		Logging:           chamberLogging.Config{},
	}
}
