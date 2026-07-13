package umoci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestPatchRootlessSpec(t *testing.T) {
	resources := &specs.LinuxResources{}
	spec := &specs.Spec{
		Process: &specs.Process{
			Args: []string{"/bin/old"},
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

	command := []string{"/bin/sh", "-c", "echo hi"}
	if err := patchRootlessSpec(spec, 501, 20, command); err != nil {
		t.Fatalf("patchRootlessSpec() error = %v", err)
	}
	command[0] = "mutated"

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
	for _, mount := range spec.Mounts {
		if mount.Type == "cgroup" || mount.Type == "cgroup2" || mount.Destination == "/sys/fs/cgroup" {
			t.Fatalf("cgroup mount still present: %#v", mount)
		}
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("Mounts length = %d, want 2", len(spec.Mounts))
	}
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

	if err := patchBundleConfig(bundlePath, 501, 20, []string{"/bin/sh"}); err != nil {
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

func TestPatchRootlessSpecKeepsExistingCommandWhenRequestCommandIsEmpty(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{Args: []string{"/bin/from-image"}},
		Linux:   &specs.Linux{},
	}

	if err := patchRootlessSpec(spec, 501, 20, nil); err != nil {
		t.Fatalf("patchRootlessSpec() error = %v", err)
	}
	if !slices.Equal(spec.Process.Args, []string{"/bin/from-image"}) {
		t.Fatalf("Process.Args = %#v, want original image args", spec.Process.Args)
	}
}

func TestPatchRootlessSpecRejectsMissingProcess(t *testing.T) {
	if err := patchRootlessSpec(&specs.Spec{Linux: &specs.Linux{}}, 501, 20, nil); err == nil {
		t.Fatal("patchRootlessSpec() error = nil, want missing process error")
	}
}

func TestValidateContainerID(t *testing.T) {
	valid := []string{
		"container-1",
		"container_1",
		"container.1",
		"C1",
	}
	for _, id := range valid {
		t.Run("valid_"+id, func(t *testing.T) {
			if err := validateContainerID(id); err != nil {
				t.Fatalf("validateContainerID(%q) error = %v", id, err)
			}
		})
	}

	invalid := []string{
		"",
		".",
		"..",
		"../escape",
		"/absolute",
		"has/slash",
		"has space",
	}
	for _, id := range invalid {
		t.Run("invalid_"+id, func(t *testing.T) {
			if err := validateContainerID(id); err == nil {
				t.Fatalf("validateContainerID(%q) error = nil, want error", id)
			}
		})
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
