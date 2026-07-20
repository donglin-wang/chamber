// Package directory provides Chamber's directory-backed OCI bundle provisioner.
// It currently uses umoci to unpack OCI image layouts before applying Chamber's
// rootless runtime spec defaults.
package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	"github.com/donglin-wang/chamber/pkg/shared/containerid"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
	"github.com/donglin-wang/chamber/pkg/shared/imageref"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	ociumoci "github.com/opencontainers/umoci"
	"github.com/opencontainers/umoci/oci/layer"
)

var _ chamberBundleShared.Provisioner = (*Provisioner)(nil)

type Provisioner struct {
	config           chamberBundleShared.Config
	uid              uint32
	gid              uint32
	directoryManager localfs.DirectoryManager
	logger           *chamberLogging.SlogLogger
}

type Option func(*Provisioner)

func WithIDMap(uid uint32, gid uint32) Option {
	return func(provisioner *Provisioner) {
		provisioner.uid = uid
		provisioner.gid = gid
	}
}

func New(config chamberBundleShared.Config, directoryManager localfs.DirectoryManager, options ...Option) (*Provisioner, error) {
	installApexBridge()

	logger, err := chamberLogging.LoggerFromConfig(config.Logging, nil)
	if err != nil {
		return nil, err
	}

	provisioner := &Provisioner{
		config:           config,
		uid:              uint32(os.Geteuid()),
		gid:              uint32(os.Getegid()),
		directoryManager: directoryManager,
		logger:           logger,
	}
	for _, option := range options {
		option(provisioner)
	}
	return provisioner, nil
}

func (p *Provisioner) Descriptor() chamberBundleShared.Descriptor {
	return chamberBundleShared.Descriptor{
		Name: chamberBundleShared.ProvisionerNameDirectory,
		Capabilities: chamberBundleShared.Capabilities{
			Privileges: []capability.Privilege{
				capability.Rootless,
			},
		},
	}
}

func (p *Provisioner) Provision(
	ctx context.Context,
	request chamberBundleShared.ProvisionRequest,
) (chamberBundleShared.ProvisionedBundle, error) {
	if err := ctx.Err(); err != nil {
		return chamberBundleShared.ProvisionedBundle{}, err
	}
	if p == nil || p.directoryManager == nil {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("%w: directory manager is required", chamberErrors.ErrInvalidRequest)
	}
	if p.config.Root == "" {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("%w: bundle root is required", chamberErrors.ErrInvalidRequest)
	}
	if err := containerid.Validate(request.ContainerID); err != nil {
		return chamberBundleShared.ProvisionedBundle{}, err
	}
	if request.ImageLayout == "" {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("%w: image layout is required", chamberErrors.ErrInvalidRequest)
	}
	if request.ImageRef == "" {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("%w: image ref is required", chamberErrors.ErrInvalidRequest)
	}
	imageRef, err := imageref.Canonical(request.ImageRef)
	if err != nil {
		return chamberBundleShared.ProvisionedBundle{}, err
	}

	bundleRoot := p.config.Root
	finalBundle := filepath.Join(bundleRoot, request.ContainerID)
	chamberLogging.InfoWith(p.logger, ctx, "provisioning bundle",
		"container_id", request.ContainerID,
		"image_ref", imageRef,
		"image_layout", request.ImageLayout,
		"bundle_path", finalBundle,
	)
	tmpBundle, err := p.directoryManager.MkdirTemp(bundleRoot, "."+request.ContainerID+".tmp-*")
	if err != nil {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("create temporary bundle: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmpBundle)
		}
	}()

	engine, err := ociumoci.OpenLayout(request.ImageLayout)
	if err != nil {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("open OCI image layout: %w", err)
	}
	defer engine.Close()

	mapOptions := layer.MapOptions{
		UIDMappings: []specs.LinuxIDMapping{{ContainerID: 0, HostID: p.uid, Size: 1}},
		GIDMappings: []specs.LinuxIDMapping{{ContainerID: 0, HostID: p.gid, Size: 1}},
		Rootless:    true,
	}
	if err := ociumoci.Unpack(engine, imageRef, tmpBundle, layer.UnpackOptions{
		OnDiskFormat: layer.DirRootfs{MapOptions: mapOptions},
	}); err != nil {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("unpack OCI image into runtime bundle: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return chamberBundleShared.ProvisionedBundle{}, err
	}

	mounts, err := normalizeBindMounts(request.Mounts)
	if err != nil {
		return chamberBundleShared.ProvisionedBundle{}, err
	}
	if err := createBindMountTargets(filepath.Join(tmpBundle, "rootfs"), mounts); err != nil {
		return chamberBundleShared.ProvisionedBundle{}, err
	}
	if err := patchBundleConfig(tmpBundle, p.uid, p.gid, request.Process, mounts); err != nil {
		return chamberBundleShared.ProvisionedBundle{}, err
	}

	if err := os.Rename(tmpBundle, finalBundle); err != nil {
		return chamberBundleShared.ProvisionedBundle{}, fmt.Errorf("commit runtime bundle: %w", err)
	}
	committed = true

	provisioned := chamberBundleShared.ProvisionedBundle{
		ContainerID: request.ContainerID,
		BundlePath:  finalBundle,
	}
	chamberLogging.InfoWith(p.logger, ctx, "provisioned bundle",
		"container_id", provisioned.ContainerID,
		"bundle_path", provisioned.BundlePath,
	)
	return provisioned, nil
}

func patchBundleConfig(bundlePath string, uid uint32, gid uint32, process chamberBundleShared.ProcessSpec, mounts []specs.Mount) error {
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

	if err := patchRootlessSpec(&spec, uid, gid, process, mounts); err != nil {
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
	process chamberBundleShared.ProcessSpec,
	mounts []specs.Mount,
) error {
	if spec == nil {
		return fmt.Errorf("%w: runtime spec is required", chamberErrors.ErrInvalidRequest)
	}
	if spec.Process == nil {
		return fmt.Errorf("%w: runtime spec process is required", chamberErrors.ErrInvalidRequest)
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
	spec.Mounts = append(spec.Mounts, cloneMounts(mounts)...)
	if process.Terminal != nil {
		spec.Process.Terminal = *process.Terminal
	}

	if len(process.Args) > 0 {
		spec.Process.Args = slices.Clone(process.Args)
	}
	if len(process.Env) > 0 {
		spec.Process.Env = slices.Clone(process.Env)
	}
	if process.Cwd != "" {
		spec.Process.Cwd = process.Cwd
	}
	applyProcessUser(spec.Process, process.User)

	return nil
}

func normalizeBindMounts(mounts []chamberBundleShared.Mount) ([]specs.Mount, error) {
	normalized := make([]specs.Mount, 0, len(mounts))
	for _, mount := range mounts {
		mountType := strings.TrimSpace(mount.Type)
		if mountType == "" {
			mountType = "bind"
		}
		if mountType != "bind" {
			return nil, fmt.Errorf("%w: unsupported rootless mount type %q", chamberErrors.ErrInvalidRequest, mount.Type)
		}

		source := strings.TrimSpace(mount.Source)
		if source == "" {
			return nil, fmt.Errorf("%w: bind mount source is required", chamberErrors.ErrInvalidRequest)
		}
		absoluteSource, err := filepath.Abs(source)
		if err != nil {
			return nil, fmt.Errorf("resolve bind mount source %q: %w", source, err)
		}
		if _, err := os.Stat(absoluteSource); err != nil {
			return nil, fmt.Errorf("stat bind mount source %q: %w", absoluteSource, err)
		}

		target := path.Clean(strings.TrimSpace(mount.Target))
		if !path.IsAbs(target) || target == "/" {
			return nil, fmt.Errorf("%w: bind mount target must be an absolute container path below root: %q", chamberErrors.ErrInvalidRequest, mount.Target)
		}

		options := slices.Clone(mount.Options)
		if len(options) == 0 {
			options = []string{"rbind", "rw"}
		}

		normalized = append(normalized, specs.Mount{
			Destination: target,
			Type:        "bind",
			Source:      absoluteSource,
			Options:     options,
		})
	}
	return normalized, nil
}

func createBindMountTargets(rootfs string, mounts []specs.Mount) error {
	for _, mount := range mounts {
		sourceInfo, err := os.Stat(mount.Source)
		if err != nil {
			return fmt.Errorf("stat bind mount source %q: %w", mount.Source, err)
		}

		targetPath, err := rootfsPath(rootfs, mount.Destination)
		if err != nil {
			return err
		}
		if sourceInfo.IsDir() {
			if err := os.MkdirAll(targetPath, 0700); err != nil {
				return fmt.Errorf("create bind mount directory target %q: %w", mount.Destination, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
			return fmt.Errorf("create bind mount file target parent %q: %w", mount.Destination, err)
		}
		if info, err := os.Stat(targetPath); err == nil {
			if info.IsDir() {
				return fmt.Errorf("bind mount target %q is a directory but source is a file", mount.Destination)
			}
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat bind mount target %q: %w", mount.Destination, err)
		}

		file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("create bind mount file target %q: %w", mount.Destination, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close bind mount file target %q: %w", mount.Destination, err)
		}
	}
	return nil
}

func rootfsPath(rootfs string, target string) (string, error) {
	cleanTarget := path.Clean(target)
	if !path.IsAbs(cleanTarget) || cleanTarget == "/" {
		return "", fmt.Errorf("%w: bind mount target must be an absolute container path below root: %q", chamberErrors.ErrInvalidRequest, target)
	}
	joined, err := securejoin.SecureJoin(rootfs, strings.TrimPrefix(cleanTarget, "/"))
	if err != nil {
		return "", fmt.Errorf("resolve bind mount target %q: %w", target, err)
	}
	return joined, nil
}

func cloneMounts(mounts []specs.Mount) []specs.Mount {
	cloned := make([]specs.Mount, len(mounts))
	for i, mount := range mounts {
		cloned[i] = mount
		cloned[i].Options = slices.Clone(mount.Options)
	}
	return cloned
}

func applyProcessUser(process *specs.Process, user chamberBundleShared.ProcessUser) {
	if user.UID != nil {
		process.User.UID = *user.UID
	}
	if user.GID != nil {
		process.User.GID = *user.GID
	}
	if user.AdditionalGIDs != nil {
		process.User.AdditionalGids = slices.Clone(user.AdditionalGIDs)
	}
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
