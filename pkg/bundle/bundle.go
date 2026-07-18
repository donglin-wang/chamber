package bundle

import (
	"context"

	"github.com/donglin-wang/chamber/pkg/shared/capability"
)

// Mount describes one filesystem mount visible inside the container. For
// ProvisionRequest.Mounts, Source is a host path and Target is an absolute
// container path.
type Mount struct {
	Type    string
	Source  string
	Target  string
	Options []string
}

type RootFS struct {
	// Mounts is empty when BundlePath/rootfs is already a populated directory.
	// Future overlayfs or snapshot-based provisioners can return mounts here
	// and leave the runtime responsible for applying and later unmounting them.
	Mounts []Mount
}

type ProvisionedBundle struct {
	ContainerID string
	BundlePath  string
	RootFS      RootFS
}

type ProvisionRequest struct {
	ContainerID string
	ImageLayout string
	ImageRef    string
	Process     ProcessSpec
	Mounts      []Mount
}

type ProcessSpec struct {
	Args     []string
	Env      []string
	Cwd      string
	User     ProcessUser
	Terminal bool
}

type ProcessUser struct {
	UID            *uint32
	GID            *uint32
	AdditionalGIDs []uint32
	Username       string
}

type Capabilities struct {
	Privileges []capability.Privilege
}

type Descriptor struct {
	Name         string
	Version      string
	Capabilities Capabilities
}

type Provisioner interface {
	Descriptor() Descriptor

	// Provision creates the OCI runtime bundle for one container. It owns image
	// unpacking, spec generation or patching, temporary staging, and the atomic
	// move into the final bundle directory.
	Provision(ctx context.Context, request ProvisionRequest) (ProvisionedBundle, error)
}
