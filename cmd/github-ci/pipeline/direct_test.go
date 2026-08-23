package pipeline

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
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("resolve user cache root: %v", err)
	}
	exitCode, err := RunDirectSDK(context.Background(), Config{
		Root:    filepath.Join(cacheRoot, "chamber", "ci-integration"),
		Workdir: repoRoot,
		Image:   DefaultImage,
		Timeout: 30 * time.Minute,
		Keep:    false,
	})
	if err != nil {
		t.Fatalf("RunDirectSDK() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
}

func TestRunRequiresRoot(t *testing.T) {
	exitCode, err := Run(context.Background(), Config{
		Workdir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Run() error = nil, want missing root error")
	}
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(err.Error(), "CI root is required") {
		t.Fatalf("Run() error = %v, want missing root error", err)
	}
}

func TestRunRejectsRootInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	exitCode, err := RunDirectSDK(context.Background(), Config{
		Root:    filepath.Join(workspace, ".chamber-ci"),
		Workdir: workspace,
	})
	if err == nil {
		t.Fatal("RunDirectSDK() error = nil, want workspace-contained root error")
	}
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(err.Error(), "must be outside workspace") {
		t.Fatalf("RunDirectSDK() error = %v, want outside workspace error", err)
	}
}

func TestGoTestCommandUsesContainerLocalGoState(t *testing.T) {
	command := strings.Join(GoTestCommand(), " ")
	for _, want := range []string{
		"GOCACHE=" + ContainerGoStateRoot + "/build",
		"GOMODCACHE=" + ContainerGoStateRoot + "/mod",
		"GOTMPDIR=" + ContainerGoStateRoot + "/work",
		"exec go test ./...",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("GoTestCommand() = %q, want %q", command, want)
		}
	}
	for _, forbidden := range []string{"/tmp", "/chamber-go-cache"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("GoTestCommand() = %q, must not use %q", command, forbidden)
		}
	}
}

func TestPathContains(t *testing.T) {
	root := t.TempDir()
	if !PathContains(root, filepath.Join(root, "child")) {
		t.Fatal("PathContains(root, child) = false, want true")
	}
	if PathContains(filepath.Join(root, "child"), root) {
		t.Fatal("PathContains(child, root) = true, want false")
	}
}
