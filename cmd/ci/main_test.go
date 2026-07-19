package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
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

func TestResolveMachineRootDefaultsOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()

	root, err := resolveMachineRoot("", workspace)
	if err != nil {
		t.Fatalf("resolveMachineRoot() error = %v", err)
	}
	if root == "" {
		t.Fatal("resolveMachineRoot() = empty, want cache root")
	}
	if pathContains(workspace, root) {
		t.Fatalf("default machine root %q is inside workspace %q", root, workspace)
	}
	if filepath.Base(root) != "machines" || filepath.Base(filepath.Dir(root)) != "chamber" {
		t.Fatalf("default machine root = %q, want path ending in chamber/machines", root)
	}
}

func TestResolveMachineRootRejectsWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".chamber-machines")

	if _, err := resolveMachineRoot(root, workspace); err == nil {
		t.Fatal("resolveMachineRoot() error = nil, want workspace root rejection")
	}
}

func TestUseMachineModes(t *testing.T) {
	if useMachine(machineModeNone) {
		t.Fatal("useMachine(none) = true, want false")
	}
	if !useMachine("custom-machine") {
		t.Fatal("useMachine(custom-machine) = false, want true")
	}
}

func TestGuestCIArgsDisableMachineRecursion(t *testing.T) {
	cfg := &config{
		image:   "docker.io/library/golang:1.26.4-bookworm",
		timeout: 45 * time.Minute,
		keep:    true,
	}

	args := guestCIArgs("/machine/bin/chamber-ci", cfg, "/workspace", "/ci-root")
	want := []string{
		"/machine/bin/chamber-ci",
		"-machine=none",
		"-workdir", "/workspace",
		"-image", "docker.io/library/golang:1.26.4-bookworm",
		"-timeout", "45m0s",
		"-keep=true",
		"-root", "/ci-root",
	}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("args[%d] = %q, want %q; args = %#v", index, args[index], want[index], args)
		}
	}
}

func TestCIMachineSpecMountsWorkspaceAndGuestRoot(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	guestRoot := filepath.Join(t.TempDir(), "guest")

	spec, err := ciMachineSpec(workspace, guestRoot, "")
	if err != nil {
		t.Fatalf("ciMachineSpec() error = %v", err)
	}
	if spec.OS != "linux" || spec.Arch == "" {
		t.Fatalf("Spec = %#v, want Linux with host arch", spec)
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("mounts = %#v, want workspace and guest root", spec.Mounts)
	}
	for _, mount := range spec.Mounts {
		if mount.Source != mount.Target {
			t.Fatalf("mount = %#v, want same source and target", mount)
		}
		if !mount.Writable {
			t.Fatalf("mount = %#v, want writable", mount)
		}
	}
	if spec.SetupScript == "" {
		t.Fatal("SetupScript = empty, want rootless sysctl setup")
	}
}

func TestCIMachineSpecSkipsMountsCoveredByParent(t *testing.T) {
	workspace := t.TempDir()
	guestRoot := filepath.Join(workspace, "guest")

	spec, err := ciMachineSpec(workspace, guestRoot, "")
	if err != nil {
		t.Fatalf("ciMachineSpec() error = %v", err)
	}
	if len(spec.Mounts) != 1 {
		t.Fatalf("mounts = %#v, want only parent workspace mount", spec.Mounts)
	}
}

func TestDefaultMachineNameIsStableAndValid(t *testing.T) {
	workspace := "/Users/donglinwang/Projects/chamber"

	first := defaultMachineName(workspace)
	second := defaultMachineName(workspace)
	if first != second {
		t.Fatalf("defaultMachineName() = %q then %q, want stable", first, second)
	}
	if len(first) != len("cci-")+8 {
		t.Fatalf("defaultMachineName() = %q, want prefix plus 8 hex chars", first)
	}
}

func TestLogResultUsesStructuredLogger(t *testing.T) {
	var buffer bytes.Buffer
	old := chamberLogging.Logger()
	chamberLogging.SetLogger(chamberLogging.NewJSONLogger(&buffer, slog.LevelInfo))
	t.Cleanup(func() {
		chamberLogging.SetLogger(old)
	})

	logResult(context.Background(), jobResult{
		name:     "pkg",
		exitCode: 0,
		stdout:   []byte("ok github.com/donglin-wang/chamber/pkg/image\n"),
	})

	entries := decodeLogEntries(t, buffer.Bytes())
	if len(entries) != 2 {
		t.Fatalf("log entries = %d, want 2; output = %s", len(entries), buffer.String())
	}
	if entries[0]["msg"] != "CI job output" {
		t.Fatalf("first msg = %v, want CI job output", entries[0]["msg"])
	}
	if entries[0]["job"] != "pkg" || entries[0]["stream"] != "stdout" {
		t.Fatalf("first entry job/stream = %v/%v, want pkg/stdout", entries[0]["job"], entries[0]["stream"])
	}
	if entries[0]["output"] != "ok github.com/donglin-wang/chamber/pkg/image\n" {
		t.Fatalf("first entry output = %v", entries[0]["output"])
	}
	if entries[1]["msg"] != "CI job passed" || entries[1]["job"] != "pkg" {
		t.Fatalf("second entry = %#v, want passed pkg event", entries[1])
	}
}

func decodeLogEntries(t *testing.T, data []byte) []map[string]any {
	t.Helper()

	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log entry %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}
