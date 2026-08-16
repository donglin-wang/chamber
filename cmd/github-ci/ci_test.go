package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const integrationFlag = "CHAMBER_INTEGRATION"

func TestRunDogfoodIntegration(t *testing.T) {
	if os.Getenv(integrationFlag) != "1" {
		t.Skipf("set %s=1 to run Chamber dogfood CI", integrationFlag)
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("resolve user cache root: %v", err)
	}
	exitCode, err := runCI(context.Background(), ciConfig{
		Root:    filepath.Join(cacheRoot, "chamber", "ci-integration"),
		Workdir: repoRoot,
		Image:   defaultCIImage,
		Timeout: 30 * time.Minute,
		Keep:    false,
	})
	if err != nil {
		t.Fatalf("runCI() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
}

func TestRunRequiresRoot(t *testing.T) {
	exitCode, err := runCI(context.Background(), ciConfig{
		Workdir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("runCI() error = nil, want missing root error")
	}
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(err.Error(), "CI root is required") {
		t.Fatalf("runCI() error = %v, want missing root error", err)
	}
}

func TestRunRejectsRootInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	exitCode, err := runCI(context.Background(), ciConfig{
		Root:    filepath.Join(workspace, ".chamber-ci"),
		Workdir: workspace,
	})
	if err == nil {
		t.Fatal("runCI() error = nil, want workspace-contained root error")
	}
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(err.Error(), "must be outside workspace") {
		t.Fatalf("runCI() error = %v, want outside workspace error", err)
	}
}

func TestGoTestCommandUsesContainerLocalGoState(t *testing.T) {
	command := strings.Join(testCommand, " ")
	for _, want := range []string{
		"GOCACHE=" + containerGoStateRoot + "/build",
		"GOMODCACHE=" + containerGoStateRoot + "/mod",
		"GOTMPDIR=" + containerGoStateRoot + "/work",
		"exec go test ./...",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("testCommand = %q, want %q", command, want)
		}
	}
	for _, forbidden := range []string{"/tmp", "/chamber-go-cache"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("testCommand = %q, must not use %q", command, forbidden)
		}
	}
}

func TestPathContains(t *testing.T) {
	root := t.TempDir()
	if !pathContains(root, filepath.Join(root, "child")) {
		t.Fatal("pathContains(root, child) = false, want true")
	}
	if pathContains(filepath.Join(root, "child"), root) {
		t.Fatal("pathContains(child, root) = true, want false")
	}
}
