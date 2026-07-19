package lima

import (
	"fmt"
	"strings"

	chamberMachine "github.com/donglin-wang/chamber/pkg/machine"
	"go.yaml.in/yaml/v2"
)

const gib = 1024 * 1024 * 1024

type limaSpec struct {
	OS         string          `yaml:"os,omitempty"`
	Arch       string          `yaml:"arch,omitempty"`
	Images     []limaImage     `yaml:"images,omitempty"`
	CPUs       int             `yaml:"cpus,omitempty"`
	Memory     string          `yaml:"memory,omitempty"`
	Disk       string          `yaml:"disk,omitempty"`
	Mounts     []limaMount     `yaml:"mounts,omitempty"`
	Provision  []limaProvision `yaml:"provision,omitempty"`
	Containerd limaContainerd  `yaml:"containerd"`
}

type limaImage struct {
	Location string `yaml:"location"`
	Arch     string `yaml:"arch"`
}

type limaMount struct {
	Location   string `yaml:"location"`
	MountPoint string `yaml:"mountPoint,omitempty"`
	Writable   bool   `yaml:"writable,omitempty"`
}

type limaProvision struct {
	Mode   string `yaml:"mode"`
	Script string `yaml:"script"`
}

type limaContainerd struct {
	System bool `yaml:"system"`
	User   bool `yaml:"user"`
}

func renderSpec(spec chamberMachine.Spec) ([]byte, error) {
	if spec.OS == "" {
		spec.OS = "linux"
	}
	if strings.ToLower(spec.OS) != "linux" {
		return nil, fmt.Errorf("Lima machine only supports Linux specs, got %q", spec.OS)
	}

	rendered := limaSpec{
		OS:         "Linux",
		Arch:       limaArch(spec.Arch),
		Images:     ubuntuImages(),
		CPUs:       spec.CPUs,
		Memory:     bytesToGiB(spec.MemoryBytes),
		Disk:       bytesToGiB(spec.DiskBytes),
		Mounts:     renderMounts(spec.Mounts),
		Containerd: limaContainerd{},
	}
	if strings.TrimSpace(spec.SetupScript) != "" {
		rendered.Provision = []limaProvision{{
			Mode:   "system",
			Script: spec.SetupScript,
		}}
	}
	data, err := yaml.Marshal(rendered)
	if err != nil {
		return nil, fmt.Errorf("encode Lima spec: %w", err)
	}
	return data, nil
}

func renderMounts(mounts []chamberMachine.Mount) []limaMount {
	rendered := make([]limaMount, 0, len(mounts))
	for _, mount := range mounts {
		rendered = append(rendered, limaMount{
			Location:   mount.Source,
			MountPoint: mount.Target,
			Writable:   mount.Writable,
		})
	}
	return rendered
}

func limaArch(arch string) string {
	switch arch {
	case "", "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return arch
	}
}

func ubuntuImages() []limaImage {
	return []limaImage{
		{
			Location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img",
			Arch:     "aarch64",
		},
		{
			Location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img",
			Arch:     "x86_64",
		},
	}
}

func bytesToGiB(value int64) string {
	if value <= 0 {
		return ""
	}
	if value%gib == 0 {
		return fmt.Sprintf("%dGiB", value/gib)
	}
	return fmt.Sprintf("%dMiB", value/(1024*1024))
}
