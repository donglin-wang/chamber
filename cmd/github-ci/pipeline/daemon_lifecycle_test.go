package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunDaemonLifecycleIntegration(t *testing.T) {
	if os.Getenv(integrationFlag) != "1" {
		t.Skipf("set %s=1 to run daemon lifecycle CI", integrationFlag)
	}
	if runtime.GOOS != "linux" {
		t.Skip("daemon lifecycle CI requires Linux")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("resolve user cache root: %v", err)
	}
	proofDir := strings.TrimSpace(os.Getenv("CHAMBER_DAEMON_LIFECYCLE_PROOF_DIR"))
	if proofDir == "" {
		proofDir = filepath.Join(cacheRoot, "chamber", "daemon-lifecycle-proof")
	}
	image := strings.TrimSpace(os.Getenv("CHAMBER_DAEMON_LIFECYCLE_IMAGE"))
	if image == "" {
		image = DefaultImage
	}
	exitCode, err := RunDaemonLifecycle(context.Background(), Config{
		Root:        filepath.Join(cacheRoot, "chamber", "ci-integration"),
		Workdir:     repoRoot,
		Image:       image,
		EvidenceDir: proofDir,
		Timeout:     45 * time.Minute,
		Keep:        false,
	})
	if err != nil {
		t.Fatalf("RunDaemonLifecycle() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	t.Logf("daemon lifecycle proof evidence: %s", proofDir)
}
