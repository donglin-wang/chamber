package directory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	ociumoci "github.com/opencontainers/umoci"
)

func TestNewAppliesIDMapOption(t *testing.T) {
	provisioner, err := New(
		chamberBundleShared.Config{
			Root:      filepath.Join(privateTempDir(t), "bundles"),
			Privilege: capability.Rootless,
		},
		localfs.NewDirectoryManager(),
		WithIDMap(123, 456),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if provisioner.uid != 123 || provisioner.gid != 456 {
		t.Fatalf("uid/gid = %d/%d, want 123/456", provisioner.uid, provisioner.gid)
	}
}

func TestDescriptorDeclaresDirectorySupport(t *testing.T) {
	provisioner := &Provisioner{}

	descriptor := provisioner.Descriptor()

	if descriptor.Name != "directory" {
		t.Fatalf("Descriptor().Name = %q, want directory", descriptor.Name)
	}
	if !slices.Equal(descriptor.Capabilities.Privileges, []capability.Privilege{capability.Rootless}) {
		t.Fatalf("privileges = %#v, want rootless only", descriptor.Capabilities.Privileges)
	}
}

func TestProvisionCanonicalizesImageRefBeforeUnpack(t *testing.T) {
	imageLayout := filepath.Join(t.TempDir(), "layout")
	engine, err := ociumoci.CreateLayout(imageLayout)
	if err != nil {
		t.Fatalf("CreateLayout() error = %v", err)
	}
	if err := ociumoci.NewImage(engine, "index.docker.io/library/golang:1.26.4-bookworm", nil); err != nil {
		t.Fatalf("NewImage() error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	provisioner, err := New(chamberBundleShared.Config{
		Root:      filepath.Join(privateTempDir(t), "bundles"),
		Privilege: capability.Rootless,
	}, localfs.NewDirectoryManager())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	provisioned, err := provisioner.Provision(context.Background(), chamberBundleShared.ProvisionRequest{
		ContainerID: "container-1",
		ImageLayout: imageLayout,
		ImageRef:    "docker.io/library/golang:1.26.4-bookworm",
	})
	if err != nil {
		if strings.Contains(err.Error(), "tag is not found") {
			t.Fatalf("Provision() error = %v, want non-canonical image ref to resolve to canonical layout tag", err)
		}
		if goruntime.GOOS == "darwin" && strings.Contains(err.Error(), "unsupported OS: darwin") {
			return
		}
		t.Fatalf("Provision() error = %v", err)
	}
	if provisioned.BundlePath == "" {
		t.Fatal("Provision() BundlePath = empty, want provisioned bundle")
	}
	if _, err := os.Stat(filepath.Join(provisioned.BundlePath, "config.json")); err != nil {
		t.Fatalf("provisioned config.json missing: %v", err)
	}
}

func TestPatchRootlessSpec(t *testing.T) {
	resources := &specs.LinuxResources{}
	spec := &specs.Spec{
		Process: &specs.Process{
			Args:     []string{"/bin/old"},
			Env:      []string{"OLD=value"},
			Cwd:      "/",
			Terminal: false,
		},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc"},
			{Destination: "/sys/fs/cgroup", Type: "cgroup"},
			{Destination: "/sys/fs/cgroup/unified", Type: "cgroup2"},
			{Destination: "/sys/fs/cgroup/nested", Type: "bind"},
			{Destination: "/data", Type: "bind"},
		},
		Linux: &specs.Linux{
			UIDMappings: []specs.LinuxIDMapping{{ContainerID: 1000, HostID: 1000, Size: 1}},
			GIDMappings: []specs.LinuxIDMapping{{ContainerID: 1000, HostID: 1000, Size: 1}},
			CgroupsPath: "chamber/container-1",
			Resources:   resources,
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.NetworkNamespace},
				{Type: specs.UserNamespace},
				{Type: specs.CgroupNamespace},
				{Type: specs.UserNamespace},
				{Type: specs.MountNamespace},
			},
		},
	}

	uid := uint32(1000)
	gid := uint32(1001)
	process := chamberBundleShared.ProcessSpec{
		Args:     []string{"/bin/sh", "-c", "echo hi"},
		Env:      []string{"KEY=value"},
		Cwd:      "/work",
		Terminal: boolPtr(true),
		User: chamberBundleShared.ProcessUser{
			UID:            &uid,
			GID:            &gid,
			AdditionalGIDs: []uint32{44, 55},
		},
	}
	if err := patchRootlessSpec(spec, 501, 20, process, nil); err != nil {
		t.Fatalf("patchRootlessSpec() error = %v", err)
	}
	process.Args[0] = "mutated"
	process.Env[0] = "mutated"
	process.User.AdditionalGIDs[0] = 99

	assertIDMappings(t, spec.Linux.UIDMappings, 501)
	assertIDMappings(t, spec.Linux.GIDMappings, 20)
	if spec.Linux.Resources != nil {
		t.Fatalf("Linux.Resources = %#v, want nil", spec.Linux.Resources)
	}
	if spec.Linux.CgroupsPath != "" {
		t.Fatalf("Linux.CgroupsPath = %q, want empty", spec.Linux.CgroupsPath)
	}
	if got := countNamespace(spec.Linux.Namespaces, specs.UserNamespace); got != 1 {
		t.Fatalf("user namespace count = %d, want 1", got)
	}
	if hasNamespace(spec.Linux.Namespaces, specs.NetworkNamespace) {
		t.Fatal("network namespace still present")
	}
	if hasNamespace(spec.Linux.Namespaces, specs.CgroupNamespace) {
		t.Fatal("cgroup namespace still present")
	}
	if !slices.Equal(spec.Process.Args, []string{"/bin/sh", "-c", "echo hi"}) {
		t.Fatalf("Process.Args = %#v, want command override", spec.Process.Args)
	}
	if !slices.Equal(spec.Process.Env, []string{"KEY=value"}) {
		t.Fatalf("Process.Env = %#v, want env override", spec.Process.Env)
	}
	if spec.Process.Cwd != "/work" {
		t.Fatalf("Process.Cwd = %q, want /work", spec.Process.Cwd)
	}
	if !spec.Process.Terminal {
		t.Fatal("Process.Terminal = false, want process override")
	}
	if spec.Process.User.UID != 1000 || spec.Process.User.GID != 1001 {
		t.Fatalf("Process.User UID/GID = %d/%d, want 1000/1001", spec.Process.User.UID, spec.Process.User.GID)
	}
	if !slices.Equal(spec.Process.User.AdditionalGids, []uint32{44, 55}) {
		t.Fatalf("Process.User.AdditionalGids = %#v, want copied groups", spec.Process.User.AdditionalGids)
	}
	for _, mount := range spec.Mounts {
		if mount.Type == "cgroup" || mount.Type == "cgroup2" || mount.Destination == "/sys/fs/cgroup" {
			t.Fatalf("cgroup mount still present: %#v", mount)
		}
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("Mounts length = %d, want 2", len(spec.Mounts))
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func TestPatchBundleConfigWritesPrivateSpec(t *testing.T) {
	bundlePath := t.TempDir()
	spec := specs.Spec{
		Process: &specs.Process{Args: []string{"/bin/from-image"}},
		Linux:   &specs.Linux{},
	}
	configPath := filepath.Join(bundlePath, "config.json")
	content, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(configPath, content, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := patchBundleConfig(bundlePath, 501, 20, chamberBundleShared.ProcessSpec{
		Args: []string{"/bin/sh"},
	}, nil); err != nil {
		t.Fatalf("patchBundleConfig() error = %v", err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config.json mode = %o, want 0600", info.Mode().Perm())
	}

	file, err := os.Open(configPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer file.Close()

	var patched specs.Spec
	if err := json.NewDecoder(file).Decode(&patched); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !slices.Equal(patched.Process.Args, []string{"/bin/sh"}) {
		t.Fatalf("Process.Args = %#v, want command override", patched.Process.Args)
	}
	assertIDMappings(t, patched.Linux.UIDMappings, 501)
	assertIDMappings(t, patched.Linux.GIDMappings, 20)
}

func TestPatchRootlessSpecKeepsExistingProcessFieldsWhenRequestFieldsAreEmpty(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{
			Args:     []string{"/bin/from-image"},
			Env:      []string{"FROM=image"},
			Cwd:      "/",
			Terminal: true,
		},
		Linux: &specs.Linux{},
	}

	if err := patchRootlessSpec(spec, 501, 20, chamberBundleShared.ProcessSpec{}, nil); err != nil {
		t.Fatalf("patchRootlessSpec() error = %v", err)
	}
	if !slices.Equal(spec.Process.Args, []string{"/bin/from-image"}) {
		t.Fatalf("Process.Args = %#v, want original image args", spec.Process.Args)
	}
	if !slices.Equal(spec.Process.Env, []string{"FROM=image"}) {
		t.Fatalf("Process.Env = %#v, want original image env", spec.Process.Env)
	}
	if spec.Process.Cwd != "/" {
		t.Fatalf("Process.Cwd = %q, want original image cwd", spec.Process.Cwd)
	}
	if !spec.Process.Terminal {
		t.Fatal("Process.Terminal = false, want original image terminal")
	}
}

func TestPatchRootlessSpecRejectsMissingProcess(t *testing.T) {
	if err := patchRootlessSpec(&specs.Spec{Linux: &specs.Linux{}}, 501, 20, chamberBundleShared.ProcessSpec{}, nil); err == nil {
		t.Fatal("patchRootlessSpec() error = nil, want missing process error")
	}
}

func TestPatchRootlessSpecAppendsBindMounts(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{Args: []string{"/bin/from-image"}},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc"},
			{Destination: "/data", Type: "bind", Source: "/existing", Options: []string{"rbind", "ro"}},
		},
		Linux: &specs.Linux{},
	}
	mounts := []specs.Mount{
		{Destination: "/workspace", Type: "bind", Source: "/host/workspace", Options: []string{"rbind", "rw"}},
	}

	if err := patchRootlessSpec(spec, 501, 20, chamberBundleShared.ProcessSpec{}, mounts); err != nil {
		t.Fatalf("patchRootlessSpec() error = %v", err)
	}
	mounts[0].Options[0] = "mutated"

	if len(spec.Mounts) != 3 {
		t.Fatalf("Mounts length = %d, want 3", len(spec.Mounts))
	}
	if spec.Mounts[1].Destination != "/data" {
		t.Fatalf("existing mount destination = %q, want /data", spec.Mounts[1].Destination)
	}
	got := spec.Mounts[2]
	if got.Destination != "/workspace" || got.Type != "bind" || got.Source != "/host/workspace" {
		t.Fatalf("appended mount = %#v, want workspace bind", got)
	}
	if !slices.Equal(got.Options, []string{"rbind", "rw"}) {
		t.Fatalf("appended mount options = %#v, want copied defaults", got.Options)
	}
}

func TestNormalizeBindMountsDefaultsAndExplicitOptions(t *testing.T) {
	sourceDir := t.TempDir()
	sourceFile := filepath.Join(t.TempDir(), "go.sum")
	if err := os.WriteFile(sourceFile, []byte("content"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	mounts, err := normalizeBindMounts([]chamberBundleShared.Mount{
		{Source: sourceDir, Target: "/workspace"},
		{Type: "bind", Source: sourceFile, Target: "/input/go.sum", Options: []string{"rbind", "ro"}},
	})
	if err != nil {
		t.Fatalf("normalizeBindMounts() error = %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mount count = %d, want 2", len(mounts))
	}
	if mounts[0].Type != "bind" || mounts[0].Destination != "/workspace" || !slices.Equal(mounts[0].Options, []string{"rbind", "rw"}) {
		t.Fatalf("default mount = %#v, want default bind mount", mounts[0])
	}
	if mounts[1].Destination != "/input/go.sum" || !slices.Equal(mounts[1].Options, []string{"rbind", "ro"}) {
		t.Fatalf("explicit mount = %#v, want explicit ro bind mount", mounts[1])
	}
}

func TestNormalizeBindMountsRejectsInvalidRequests(t *testing.T) {
	sourceDir := t.TempDir()
	tests := map[string]chamberBundleShared.Mount{
		"missing source": {Source: filepath.Join(sourceDir, "missing"), Target: "/workspace"},
		"empty source":   {Target: "/workspace"},
		"relative target": {
			Source: sourceDir,
			Target: "workspace",
		},
		"root target": {
			Source: sourceDir,
			Target: "/",
		},
		"unsupported type": {
			Type:   "tmpfs",
			Source: sourceDir,
			Target: "/workspace",
		},
	}

	for name, mount := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeBindMounts([]chamberBundleShared.Mount{mount}); err == nil {
				t.Fatal("normalizeBindMounts() error = nil, want error")
			}
		})
	}
}

func TestCreateBindMountTargetsCreatesRootfsPlaceholders(t *testing.T) {
	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(rootfs, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	sourceDir := t.TempDir()
	sourceFile := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(sourceFile, []byte("module example\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	mounts, err := normalizeBindMounts([]chamberBundleShared.Mount{
		{Source: sourceDir, Target: "/workspace"},
		{Source: sourceFile, Target: "/inputs/go.mod"},
	})
	if err != nil {
		t.Fatalf("normalizeBindMounts() error = %v", err)
	}
	if err := createBindMountTargets(rootfs, mounts); err != nil {
		t.Fatalf("createBindMountTargets() error = %v", err)
	}

	workspaceInfo, err := os.Stat(filepath.Join(rootfs, "workspace"))
	if err != nil {
		t.Fatalf("Stat(workspace) error = %v", err)
	}
	if !workspaceInfo.IsDir() {
		t.Fatal("workspace target is not a directory")
	}
	inputInfo, err := os.Stat(filepath.Join(rootfs, "inputs", "go.mod"))
	if err != nil {
		t.Fatalf("Stat(go.mod) error = %v", err)
	}
	if inputInfo.IsDir() {
		t.Fatal("go.mod target is a directory, want file placeholder")
	}
}

func TestRootfsPathRejectsEscapes(t *testing.T) {
	rootfs := t.TempDir()
	if _, err := rootfsPath(rootfs, "/"); err == nil {
		t.Fatal("rootfsPath(/) error = nil, want error")
	}
	if _, err := rootfsPath(rootfs, "workspace"); err == nil {
		t.Fatal("rootfsPath(relative) error = nil, want error")
	}
}

func assertIDMappings(t *testing.T, mappings []specs.LinuxIDMapping, hostID uint32) {
	t.Helper()

	want := []specs.LinuxIDMapping{{ContainerID: 0, HostID: hostID, Size: 1}}
	if !slices.Equal(mappings, want) {
		t.Fatalf("ID mappings = %#v, want %#v", mappings, want)
	}
}

func countNamespace(namespaces []specs.LinuxNamespace, namespaceType specs.LinuxNamespaceType) int {
	count := 0
	for _, namespace := range namespaces {
		if namespace.Type == namespaceType {
			count++
		}
	}
	return count
}

func hasNamespace(namespaces []specs.LinuxNamespace, namespaceType specs.LinuxNamespaceType) bool {
	return countNamespace(namespaces, namespaceType) > 0
}

func boolPtr(value bool) *bool {
	return &value
}
