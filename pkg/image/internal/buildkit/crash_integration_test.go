package buildkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	"github.com/donglin-wang/chamber/pkg/shared/hostfs"
	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
	"github.com/donglin-wang/chamber/pkg/shared/testutil"
)

func TestBuildParentDeathIntegration(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 for Linux parent-death coverage")
	}
	if root := os.Getenv("CHAMBER_CRASH_ROOT"); root != "" {
		workspace, err := hostfs.NewWorkspace(hostfs.Config{Root: filepath.Join(root, "images"), TmpRoot: filepath.Join(root, "tmp")})
		if err != nil {
			t.Fatal(err)
		}
		builder, err := New(context.Background(), chamberImage.BuildKitConfig{}, workspace, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = builder.Build(context.Background(), chamberImage.BuildRequest{Reference: "chamber.local/crash:test", ContextPath: filepath.Join(root, "context")}, filepath.Join(root, "tmp", "output-layout"))
		t.Fatalf("build unexpectedly finished before parent death: %v", err)
	}
	// Keep the Unix socket path short even when TMPDIR is below a user's home.
	root, err := os.MkdirTemp("", "ch-crash-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Base(root) + "-payload"
	reference := os.Getenv("CHAMBER_CRASH_IMAGE")
	if reference == "" {
		reference = "docker.io/library/busybox:1.36.1"
	}
	dockerfile := "FROM " + reference + "\nRUN echo " + token + "; while :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte(dockerfile), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(root, "helper.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	child := subprocess.CommandContext(ctx, executable, "-test.run=^TestBuildParentDeathIntegration$")
	child.Env = append(os.Environ(), "CHAMBER_CRASH_ROOT="+root)
	child.Stdout, child.Stderr = output, output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() {
		if t.Failed() {
			data, _ := os.ReadFile(output.Name())
			t.Logf("helper output:\n%s", data)
		}
	}()
	var payload []int
	testutil.Eventually(t, 3*time.Minute, func() bool {
		select {
		case err := <-done:
			t.Fatalf("build helper exited before readiness: %v", err)
		default:
		}
		payload = testutil.LiveProcessesMatching(token)
		return len(payload) > 0
	})
	pids := append(testutil.LiveProcessesMatching(root), payload...)
	defer func() {
		if t.Failed() {
			for _, pid := range pids {
				if testutil.ProcessAlive(pid) {
					cmdline, _ := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "cmdline"))
					t.Logf("surviving process %d: %q", pid, cmdline)
				}
			}
		}
	}()
	if len(pids) < 3 {
		t.Fatalf("expected worker/client/payload processes, got %v", pids)
	}
	// Keep teardown effective even if this regression test finds surviving children.
	t.Cleanup(func() {
		for _, pid := range pids {
			if testutil.ProcessAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	testutil.Eventually(t, 15*time.Second, func() bool {
		for _, pid := range pids {
			if testutil.ProcessAlive(pid) {
				return false
			}
		}
		return true
	})
	retained, err := filepath.Glob(filepath.Join(root, "tmp", ".bk-*"))
	if err != nil || len(retained) == 0 {
		t.Fatalf("expected caller-owned crash remnants, got %v, %v", retained, err)
	}
	t.Logf("parent SIGKILL: worker/client/payload=%v stopped; retained directories=%v", pids, retained)
}
