package image

import (
	"path/filepath"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

// Config is the final caller-provided configuration for image operations.
type Config struct {
	// Root is the private directory where the image store keeps its shared OCI
	// image layout, metadata, and temporary operation directories.
	Root string

	// BuildKit configures Dockerfile builds. A zero value uses managed BuildKit,
	// RootlessKit, and runc binaries below <Config.Root>/bin.
	BuildKit BuildKitConfig

	// Buildah configures the deprecated Buildah-backed builder. It is retained
	// for source compatibility; Store.Build uses BuildKit.
	//
	// Deprecated: configure BuildKit instead.
	Buildah BuildahConfig

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

// BuildahConfig configures Buildah-backed image builds.
//
// Deprecated: Store.Build uses BuildKitConfig. This type is retained only so
// older callers can compile while moving to BuildKitConfig.
type BuildahConfig struct {
	// Path was an absolute path to an existing buildah-worker executable.
	Path string

	// Version was the configured Buildah version for logs and diagnostics when
	// URL and SHA256 were set.
	Version string

	// URL was the source for a managed buildah-worker binary download.
	URL string

	// SHA256 was the expected hex-encoded SHA256 digest for a managed
	// buildah-worker binary.
	SHA256 string

	// StorageDriver selected the containers/storage graph driver used by
	// Buildah.
	StorageDriver string

	// Runtime was passed to Buildah for Dockerfile RUN instructions.
	Runtime string

	// Isolation was passed to Buildah for Dockerfile RUN instructions.
	Isolation string
}

// DefaultConfig returns image configuration rooted below rootPath.
func DefaultConfig(rootPath string) Config {
	imageRoot := filepath.Join(rootPath, "images")
	return Config{
		Root:     imageRoot,
		BuildKit: BuildKitConfig{},
		Buildah:  BuildahConfig{},
		Logging:  chamberLogging.Config{},
	}
}
