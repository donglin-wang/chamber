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

	// Buildah configures Dockerfile builds. A zero value uses the managed
	// buildah-worker download metadata and the vfs storage driver.
	Buildah BuildahConfig

	// Logging configures host-side Chamber logs for image operations. A zero
	// value inherits the package logger.
	Logging chamberLogging.Config
}

// BuildahConfig configures Buildah-backed image builds.
type BuildahConfig struct {
	// Path is an absolute path to an existing buildah-worker executable. Empty
	// lets Chamber download and cache the managed worker below <Config.Root>/bin.
	Path string

	// Version is the configured Buildah version for logs and diagnostics when
	// URL and SHA256 are set.
	Version string

	// URL is the source for a managed buildah-worker binary download. It must
	// point to a buildah-worker executable. Empty uses Chamber's default worker
	// release URL for the host architecture.
	URL string

	// SHA256 is the expected hex-encoded SHA256 digest for a managed
	// buildah-worker binary. URL and SHA256 must be configured together for
	// downloads.
	SHA256 string

	// StorageDriver selects the containers/storage graph driver used by
	// Buildah. Empty defaults to vfs.
	StorageDriver string

	// Runtime is passed to Buildah for Dockerfile RUN instructions when
	// non-empty. Empty lets Buildah choose its default OCI runtime.
	Runtime string

	// Isolation is passed to Buildah for Dockerfile RUN instructions. Empty
	// defaults to chroot to avoid depending on an ambient OCI runtime.
	Isolation string
}

// DefaultConfig returns image configuration rooted below rootPath.
func DefaultConfig(rootPath string) Config {
	imageRoot := filepath.Join(rootPath, "images")
	return Config{
		Root:    imageRoot,
		Buildah: BuildahConfig{},
		Logging: chamberLogging.Config{},
	}
}
