package umoci

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	chbundle "github.com/donglin-wang/chamber/internal/bundle"
	"github.com/donglin-wang/chamber/internal/shared/localfs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	ociumoci "github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/layer"
)

var containerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var _ chbundle.Provisioner = (*Provisioner)(nil)

type Provisioner struct {
	ContainerRoot    string
	UID              uint32
	GID              uint32
	DirectoryManager localfs.DirectoryManager
}

func (p Provisioner) Provision(
	ctx context.Context,
	request chbundle.ProvisionRequest,
) (chbundle.ProvisionedBundle, error) {
	if err := ctx.Err(); err != nil {
		return chbundle.ProvisionedBundle{}, err
	}
	if p.DirectoryManager == nil {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("directory manager is required")
	}
	if err := validateContainerID(request.ContainerID); err != nil {
		return chbundle.ProvisionedBundle{}, err
	}
	if p.ContainerRoot == "" {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("container root is required")
	}
	if request.ImageLayout == "" {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("image layout is required")
	}
	if request.ImageRef == "" {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("image ref is required")
	}

	containerRoot, err := filepath.Abs(p.ContainerRoot)
	if err != nil {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("resolve container root: %w", err)
	}
	if err := p.DirectoryManager.EnsurePrivateDir(containerRoot); err != nil {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("prepare container root: %w", err)
	}

	finalBundle := filepath.Join(containerRoot, request.ContainerID)
	tmpBundle, err := p.DirectoryManager.MkdirTemp(containerRoot, "."+request.ContainerID+".tmp-*")
	if err != nil {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("create temporary bundle: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmpBundle)
		}
	}()

	engine, err := ociumoci.OpenLayout(request.ImageLayout)
	if err != nil {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("open OCI image layout: %w", err)
	}
	defer engine.Close()

	mapOptions := layer.MapOptions{
		UIDMappings: []specs.LinuxIDMapping{{ContainerID: 0, HostID: p.UID, Size: 1}},
		GIDMappings: []specs.LinuxIDMapping{{ContainerID: 0, HostID: p.GID, Size: 1}},
		Rootless:    true,
	}
	if err := ociumoci.Unpack(engine, request.ImageRef, tmpBundle, layer.UnpackOptions{
		OnDiskFormat: layer.DirRootfs{MapOptions: mapOptions},
	}); err != nil {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("unpack OCI image into runtime bundle: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return chbundle.ProvisionedBundle{}, err
	}

	if err := patchBundleConfig(tmpBundle, p.UID, p.GID, request.Command); err != nil {
		return chbundle.ProvisionedBundle{}, err
	}

	if err := os.Rename(tmpBundle, finalBundle); err != nil {
		return chbundle.ProvisionedBundle{}, fmt.Errorf("commit runtime bundle: %w", err)
	}
	committed = true

	return chbundle.ProvisionedBundle{
		ContainerID: request.ContainerID,
		BundlePath:  finalBundle,
		RootFS:      chbundle.RootFS{},
	}, nil
}

func patchBundleConfig(bundlePath string, uid uint32, gid uint32, command []string) error {
	configPath := filepath.Join(bundlePath, "config.json")
	file, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("open runtime spec: %w", err)
	}

	var spec specs.Spec
	decodeErr := json.NewDecoder(file).Decode(&spec)
	closeErr := file.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode runtime spec: %w", decodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close runtime spec: %w", closeErr)
	}

	if err := patchRootlessSpec(&spec, uid, gid, command); err != nil {
		return err
	}

	output, err := json.MarshalIndent(spec, "", "\t")
	if err != nil {
		return fmt.Errorf("encode runtime spec: %w", err)
	}
	output = append(output, '\n')
	if err := os.WriteFile(configPath, output, 0600); err != nil {
		return fmt.Errorf("write runtime spec: %w", err)
	}
	if err := os.Chmod(configPath, 0600); err != nil {
		return fmt.Errorf("set runtime spec mode: %w", err)
	}

	return nil
}

func patchRootlessSpec(
	spec *specs.Spec,
	uid uint32,
	gid uint32,
	command []string,
) error {
	if spec == nil {
		return fmt.Errorf("runtime spec is required")
	}
	if spec.Process == nil {
		return fmt.Errorf("runtime spec process is required")
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}

	spec.Linux.UIDMappings = []specs.LinuxIDMapping{{ContainerID: 0, HostID: uid, Size: 1}}
	spec.Linux.GIDMappings = []specs.LinuxIDMapping{{ContainerID: 0, HostID: gid, Size: 1}}
	spec.Linux.Namespaces = patchNamespaces(spec.Linux.Namespaces)
	spec.Linux.CgroupsPath = ""
	spec.Linux.Resources = nil
	spec.Mounts = removeCgroupMounts(spec.Mounts)
	spec.Process.Terminal = false

	if len(command) > 0 {
		spec.Process.Args = slices.Clone(command)
	}

	return nil
}

func patchNamespaces(namespaces []specs.LinuxNamespace) []specs.LinuxNamespace {
	patched := make([]specs.LinuxNamespace, 0, len(namespaces)+1)
	hasUserNamespace := false
	for _, namespace := range namespaces {
		switch namespace.Type {
		case specs.NetworkNamespace, specs.CgroupNamespace:
			continue
		case specs.UserNamespace:
			if hasUserNamespace {
				continue
			}
			hasUserNamespace = true
		}
		patched = append(patched, namespace)
	}
	if !hasUserNamespace {
		patched = append(patched, specs.LinuxNamespace{Type: specs.UserNamespace})
	}
	return patched
}

func removeCgroupMounts(mounts []specs.Mount) []specs.Mount {
	patched := mounts[:0]
	for _, mount := range mounts {
		if isCgroupMount(mount) {
			continue
		}
		patched = append(patched, mount)
	}
	return patched
}

func isCgroupMount(mount specs.Mount) bool {
	return mount.Type == "cgroup" ||
		mount.Type == "cgroup2" ||
		mount.Destination == "/sys/fs/cgroup" ||
		strings.HasPrefix(mount.Destination, "/sys/fs/cgroup/")
}

func validateContainerID(id string) error {
	if !containerIDPattern.MatchString(id) || id == "." || id == ".." {
		return fmt.Errorf("unsafe container id %q", id)
	}
	return nil
}
