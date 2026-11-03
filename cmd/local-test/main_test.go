package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestConfigureNestedTestContainerPreservesUnrelatedSettings(t *testing.T) {
	bundlePath := t.TempDir()
	spec := specs.Spec{
		Process: &specs.Process{
			Args:            []string{"go", "test", "./pkg/..."},
			User:            specs.User{UID: 1000, GID: 1000},
			NoNewPrivileges: true,
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    []string{"CAP_KILL"},
				Effective:   []string{"CAP_KILL"},
				Permitted:   []string{"CAP_KILL"},
				Inheritable: []string{"CAP_KILL"},
				Ambient:     []string{"CAP_KILL"},
			},
		},
		Linux: &specs.Linux{
			MaskedPaths:   []string{"/proc/kcore", "/sys/firmware", "/process"},
			ReadonlyPaths: []string{"/proc", "/proc/sys", "/sys/other"},
			Namespaces:    []specs.LinuxNamespace{{Type: specs.UserNamespace}, {Type: specs.MountNamespace}},
			UIDMappings:   []specs.LinuxIDMapping{{ContainerID: 0, HostID: 501, Size: 1}, {ContainerID: 1, HostID: 100000, Size: 65536}},
			GIDMappings:   []specs.LinuxIDMapping{{ContainerID: 0, HostID: 1000, Size: 1}, {ContainerID: 1, HostID: 100000, Size: 65536}},
		},
		Root: &specs.Root{Path: "rootfs"},
	}
	path := filepath.Join(bundlePath, "config.json")
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Reapplying the runner policy must not accumulate capability entries.
	for range 2 {
		if err := configureNestedTestContainer(bundlePath); err != nil {
			t.Fatal(err)
		}
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var configured specs.Spec
	if err := json.Unmarshal(data, &configured); err != nil {
		t.Fatal(err)
	}
	if configured.Process.NoNewPrivileges {
		t.Fatal("mapping helpers cannot gain their file capabilities")
	}
	for _, capability := range []string{"CAP_KILL", "CAP_SETUID", "CAP_SETGID"} {
		if !slices.Contains(configured.Process.Capabilities.Bounding, capability) {
			t.Errorf("capability bound lacks %s", capability)
		}
	}
	if len(configured.Process.Capabilities.Bounding) != 3 {
		t.Fatalf("unexpected capability bounds: %v", configured.Process.Capabilities.Bounding)
	}
	if !reflect.DeepEqual(configured.Linux.MaskedPaths, []string{"/sys/firmware", "/process"}) ||
		!reflect.DeepEqual(configured.Linux.ReadonlyPaths, []string{"/sys/other"}) {
		t.Fatalf("unexpected nested proc policy: %+v", configured.Linux)
	}
	// All other process permissions, namespace mappings, and rootfs settings
	// remain exactly as provisioned.
	configured.Process.NoNewPrivileges = spec.Process.NoNewPrivileges
	configured.Process.Capabilities.Bounding = spec.Process.Capabilities.Bounding
	configured.Linux.MaskedPaths = spec.Linux.MaskedPaths
	configured.Linux.ReadonlyPaths = spec.Linux.ReadonlyPaths
	if !reflect.DeepEqual(configured, spec) {
		t.Fatalf("runner changed unrelated container settings: %+v", configured)
	}
}
