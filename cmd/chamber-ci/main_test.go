package main

import (
	"path/filepath"
	"testing"
)

func TestResolveRootDefaultsOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()

	root, err := resolveRoot("", workspace)
	if err != nil {
		t.Fatalf("resolveRoot() error = %v", err)
	}
	if root == "" {
		t.Fatal("resolveRoot() = empty, want cache root")
	}
	if pathContains(workspace, root) {
		t.Fatalf("default root %q is inside workspace %q", root, workspace)
	}
	if filepath.Base(root) != "ci" || filepath.Base(filepath.Dir(root)) != "chamber" {
		t.Fatalf("default root = %q, want path ending in chamber/ci", root)
	}
}

func TestResolveRootRejectsWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".chamber-ci")

	if _, err := resolveRoot(root, workspace); err == nil {
		t.Fatal("resolveRoot() error = nil, want workspace root rejection")
	}
}

func TestResolveRootAllowsExternalRoot(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(t.TempDir(), "chamber-ci")

	resolved, err := resolveRoot(root, workspace)
	if err != nil {
		t.Fatalf("resolveRoot() error = %v", err)
	}
	if resolved != root {
		t.Fatalf("resolveRoot() = %q, want %q", resolved, root)
	}
}
