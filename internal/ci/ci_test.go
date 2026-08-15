package ci

import (
	"context"
	"os"
	"path/filepath"
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
	exitCode, err := Run(context.Background(), Config{
		Workdir: repoRoot,
		Image:   DefaultImage,
		Timeout: 30 * time.Minute,
		Keep:    false,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
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
