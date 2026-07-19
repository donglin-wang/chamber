package machine

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestResolveRequiresRoot(t *testing.T) {
	_, err := Resolve(Config{Name: "chamber"}, Override{})
	if !errors.Is(err, ErrRootRequired) {
		t.Fatalf("Resolve() error = %v, want ErrRootRequired", err)
	}
}

func TestResolveRequiresValidName(t *testing.T) {
	root := t.TempDir()

	tests := []string{
		"",
		" chamber",
		"Chamber",
		"-chamber",
		"chamber_1",
		"chamber-",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(Config{Root: root, Name: name}, Override{})
			if err == nil {
				t.Fatal("Resolve() error = nil, want invalid name error")
			}
		})
	}
}

func TestResolveDefaultsSpecAndAbsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "machines")

	resolved, err := Resolve(Config{Root: root, Name: "chamber-ci"}, Override{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Root != root {
		t.Fatalf("Root = %q, want %q", resolved.Root, root)
	}
	if resolved.Spec.OS != "linux" {
		t.Fatalf("Spec.OS = %q, want linux", resolved.Spec.OS)
	}
	if resolved.Spec.Arch == "" {
		t.Fatal("Spec.Arch = empty, want host default")
	}
}

func TestResolveAppliesOverride(t *testing.T) {
	root := t.TempDir()
	name := "override"
	start := true
	spec := Spec{OS: "linux", Arch: "amd64", CPUs: 2}

	resolved, err := Resolve(Config{
		Root: t.TempDir(),
		Name: "base",
	}, Override{
		Root:  &root,
		Name:  &name,
		Spec:  &spec,
		Start: &start,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Root != root || resolved.Name != name || !resolved.Start {
		t.Fatalf("Resolve() = %#v, want override values", resolved)
	}
	if resolved.Spec.Arch != "amd64" || resolved.Spec.CPUs != 2 {
		t.Fatalf("Spec = %#v, want override spec", resolved.Spec)
	}
}
