package image

import (
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

// Config is the final caller-provided configuration for image operations.
type Config struct {
	// Root is the private directory where the image store keeps its shared OCI
	// image layout, metadata, and temporary operation directories.
	Root string

	// TmpRoot is the private directory where the image store keeps temporary
	// operation directories. Empty uses a user-scoped directory below the host
	// temporary directory.
	TmpRoot string

	// BuildKit configures Dockerfile builds. A zero value uses managed BuildKit,
	// RootlessKit, and runc binaries below <Config.Root>/bin.
	BuildKit BuildKitConfig

	// Logging configures host-side Chamber logs for image operations. A zero
	// value inherits the package logger.
	Logging chamberLogging.Config
}

// BuildKitConfig configures BuildKit-backed image builds.
type BuildKitConfig struct {
	// BuildctlPath is an absolute path to an existing buildctl executable.
	// Empty lets Chamber download and cache managed BuildKit tools below
	// <Config.Root>/bin.
	BuildctlPath string

	// BuildkitdPath is an absolute path to an existing buildkitd executable.
	// Empty lets Chamber download and cache managed BuildKit tools below
	// <Config.Root>/bin.
	BuildkitdPath string

	// RootlessKitPath is an absolute path to an existing rootlesskit executable.
	// Empty lets Chamber download and cache managed RootlessKit below
	// <Config.Root>/bin.
	RootlessKitPath string

	// RuncPath is an absolute path to an existing runc executable. Empty lets
	// Chamber download and cache managed runc below <Config.Root>/bin.
	RuncPath string

	// BuildKitVersion is the configured BuildKit version for logs and
	// diagnostics when BuildKitURL and BuildKitSHA256 are set. Empty uses
	// Chamber's pinned default for the host architecture.
	BuildKitVersion string

	// BuildKitURL is the source for a managed BuildKit release tarball. Empty
	// uses Chamber's pinned default for the host architecture.
	BuildKitURL string

	// BuildKitSHA256 is the expected hex-encoded SHA256 digest for the managed
	// BuildKit release tarball.
	BuildKitSHA256 string

	// RootlessKitVersion is the configured RootlessKit version for logs and
	// diagnostics when RootlessKitURL and RootlessKitSHA256 are set. Empty uses
	// Chamber's pinned default for the host architecture.
	RootlessKitVersion string

	// RootlessKitURL is the source for a managed RootlessKit release tarball.
	// Empty uses Chamber's pinned default for the host architecture.
	RootlessKitURL string

	// RootlessKitSHA256 is the expected hex-encoded SHA256 digest for the
	// managed RootlessKit release tarball.
	RootlessKitSHA256 string

	// Snapshotter selects BuildKit's OCI worker snapshotter. Empty defaults to
	// native.
	Snapshotter string
}

// DefaultConfig returns image configuration rooted below rootPath.
func DefaultConfig(rootPath string) Config {
	imageRoot := filepath.Join(rootPath, "images")
	return Config{
		Root:     imageRoot,
		TmpRoot:  hostfs.DefaultTmpRoot("images"),
		BuildKit: BuildKitConfig{},
		Logging:  chamberLogging.Config{},
	}
}
