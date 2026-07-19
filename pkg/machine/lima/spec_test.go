package lima

import (
	"strings"
	"testing"

	chamberMachine "github.com/donglin-wang/chamber/pkg/machine"
)

func TestRenderSpecMapsGenericMachineSpecToLimaYAML(t *testing.T) {
	data, err := renderSpec(chamberMachine.Spec{
		OS:          "linux",
		Arch:        "arm64",
		CPUs:        2,
		MemoryBytes: 4 * gib,
		DiskBytes:   32 * gib,
		Mounts: []chamberMachine.Mount{
			{Source: "/host/workspace", Target: "/workspace", Writable: true},
		},
		SetupScript: "#!/bin/bash\nset -eux\nsysctl -w user.max_user_namespaces=28633 || true\n",
	})
	if err != nil {
		t.Fatalf("renderSpec() error = %v", err)
	}
	yaml := string(data)
	for _, want := range []string{
		"os: Linux",
		"arch: aarch64",
		"cpus: 2",
		"memory: 4GiB",
		"disk: 32GiB",
		"location: /host/workspace",
		"mountPoint: /workspace",
		"writable: true",
		"mode: system",
		"containerd:",
		"system: false",
		"user: false",
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("rendered YAML missing %q:\n%s", want, yaml)
		}
	}
}

func TestRenderSpecRejectsNonLinuxSpec(t *testing.T) {
	_, err := renderSpec(chamberMachine.Spec{OS: "darwin"})
	if err == nil {
		t.Fatal("renderSpec() error = nil, want non-Linux rejection")
	}
}
