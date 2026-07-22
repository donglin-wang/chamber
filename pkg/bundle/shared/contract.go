package shared

import (
	"context"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
)

// Mount describes one filesystem mount visible inside the container. For
// ProvisionRequest.Mounts, Source is a host path and Target is an absolute
// container path.
type Mount struct {
	// Type is the OCI mount type. Empty defaults to "bind" for current bundle
	// provisioning.
	Type string

	// Source is the host path to mount.
	Source string

	// Target is the absolute path where the mount appears inside the container.
	Target string

	// Options are OCI mount options. Empty uses the provisioner's bind-mount
	// defaults.
	Options []string
}

// ProvisionedBundle describes a ready OCI runtime bundle.
type ProvisionedBundle struct {
	// ContainerID is the validated container ID associated with this bundle.
	ContainerID string

	// BundlePath is the final bundle directory passed to a runtime.
	BundlePath string
}

// ProvisionRequest describes one image-to-bundle provisioning operation.
type ProvisionRequest struct {
	// ContainerID is the caller-chosen ID for the bundle and eventual container.
	ContainerID string

	// ImageLayout is the path to a valid OCI image layout.
	ImageLayout string

	// ImageRef is the image reference expected in the layout metadata.
	ImageRef string

	// Process overrides the image's default process fields when set.
	Process ProcessSpec

	// Mounts are provisioner-applied mounts to add to the bundle spec.
	Mounts []Mount
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

// Capabilities describes the static support declared by a provisioner
// implementation.
type Capabilities struct {
	// Privileges lists supported host privilege modes.
	Privileges []capability.Privilege
}

// CloneCapabilities returns a deep copy of capabilities.
func CloneCapabilities(capabilities Capabilities) Capabilities {
	return Capabilities{
		Privileges: append([]capability.Privilege(nil), capabilities.Privileges...),
	}
}

// Descriptor identifies a ready provisioner implementation and its capabilities.
type Descriptor struct {
	// Name is the provisioner implementation name.
	Name string

	// Version is the implementation version when one is available.
	Version string

	// Capabilities is a copy of the provisioner's declared support.
	Capabilities Capabilities
}

// Provisioner creates OCI runtime bundles from pulled OCI image layouts.
type Provisioner interface {
	// Descriptor returns implementation identity and static capabilities.
	Descriptor() Descriptor

	// Provision creates the OCI runtime bundle for one container. It owns image
	// unpacking, spec generation or patching, temporary staging, and the atomic
	// move into the final bundle directory.
	Provision(ctx context.Context, request ProvisionRequest) (ProvisionedBundle, error)
}
