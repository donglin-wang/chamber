package bundle

import (
	"context"
	"fmt"
	"regexp"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

var validBundleID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Mount describes one filesystem mount visible inside the container. For
// ProvisionRequest.Mounts, Source is a host path and Target is an absolute
// container path.
type Mount struct {
	// Type is the OCI mount type. Empty defaults to "bind" for current bundle
	// provisioning.
	Type string `json:"type,omitempty"`

	// Source is the host path to mount.
	Source string `json:"source"`

	// Target is the absolute path where the mount appears inside the container.
	Target string `json:"target"`

	// Options are OCI mount options. Empty uses the provisioner's bind-mount
	// defaults.
	Options []string `json:"options,omitempty"`
}

// ProvisionedBundle identifies a ready OCI runtime bundle and the
// provisioner-owned storage lifecycle associated with it. BundleID is not a
// runtime container ID; callers may choose the same value for both concepts
// when they intentionally have a one-to-one relationship.
type ProvisionedBundle struct {
	// BundleID is the stable identity used by the provisioner for staging,
	// recovery, and removal across storage implementations. A provisioner may
	// use it to find implementation-private state such as overlay layers,
	// snapshots, or mount records.
	BundleID string

	// BundlePath is the final bundle directory passed to a runtime.
	BundlePath string
}

// ProvisionRequest describes one image-to-bundle provisioning operation.
type ProvisionRequest struct {
	// BundleID is the caller-chosen storage identity for this bundle. It is
	// independent from any runtime container ID.
	BundleID string

	// ImageLayout is the path to a valid OCI image layout.
	ImageLayout string

	// ImageRef is the image reference expected in the layout metadata.
	ImageRef string

	// ImageDigest is the selected image target descriptor digest when known.
	ImageDigest string

	// ImagePlatform is the selected image platform when known.
	ImagePlatform chamberImage.Platform

	// Process overrides the image's default process fields when set.
	Process ProcessSpec

	// Mounts are provisioner-applied mounts to add to the bundle spec.
	Mounts []Mount
}

// ValidateBundleID checks that id can safely identify provisioner-owned paths
// and durable storage resources.
func ValidateBundleID(id string) error {
	if !validBundleID.MatchString(id) || id == "." || id == ".." {
		return fmt.Errorf("%w: invalid bundle ID %q", chamberErrors.ErrInvalidBundleID, id)
	}
	return nil
}

// ProcessSpec describes optional process overrides for the OCI runtime spec.
type ProcessSpec struct {
	// Args replaces the image's default process arguments when non-empty.
	Args []string

	// Env replaces the process environment when non-empty.
	Env []string

	// Cwd replaces the process working directory when non-empty.
	Cwd string

	// User applies per-field process user overrides.
	User ProcessUser

	// Terminal overrides process.terminal when non-nil. Use a pointer so callers
	// can distinguish "leave the image default" from "force true" or
	// "force false".
	Terminal *bool
}

// ProcessUser describes optional process user overrides.
type ProcessUser struct {
	// UID overrides the process UID when non-nil.
	UID *uint32

	// GID overrides the process GID when non-nil.
	GID *uint32

	// AdditionalGIDs replaces the process's additional group IDs when non-empty.
	AdditionalGIDs []uint32
}

// Descriptor identifies a ready provisioner implementation.
type Descriptor struct {
	// Name is the provisioner implementation name.
	Name string

	// Version is the implementation version when one is available.
	Version string
}

// Provisioner creates OCI runtime bundles from pulled OCI image layouts.
type Provisioner interface {
	// Descriptor returns implementation identity.
	Descriptor() Descriptor

	// Provision creates a runnable OCI bundle view. It owns image unpacking,
	// spec generation or patching, temporary staging, and any storage resources
	// needed to make BundlePath ready for a runtime.
	Provision(ctx context.Context, request ProvisionRequest) (ProvisionedBundle, error)

	// Remove releases all provisioner-owned resources for a bundle, including
	// mounts, snapshots, layers, and runtime-mutated rootfs contents when the
	// implementation uses them. Missing bundles are treated as already removed.
	Remove(ctx context.Context, bundle ProvisionedBundle) error
}
