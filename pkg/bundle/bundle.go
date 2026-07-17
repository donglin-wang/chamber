package bundle

import "context"

// Mount describes one filesystem mount that must exist before the runtime
// starts the container. Target is relative to the bundle's rootfs directory;
// an empty target means the rootfs directory itself.
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

type Provisioner interface {
	// Provision creates the OCI runtime bundle for one container. It owns image
	// unpacking, spec generation or patching, temporary staging, and the atomic
	// move into the final bundle directory.
	Provision(ctx context.Context, request ProvisionRequest) (ProvisionedBundle, error)
}
