package factory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
	"github.com/donglin-wang/chamber/pkg/shared/testutil"
)

func TestRuntimeParentDeathIntegration(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 for Linux parent-death coverage")
	}
	if root := os.Getenv("CHAMBER_CRASH_ROOT"); root != "" {
		ctx := context.Background()
		store, err := chamberImageFactory.NewStore(chamberImage.DefaultConfig(root))
		if err != nil {
			t.Fatal(err)
		}
		reference := os.Getenv("CHAMBER_CRASH_IMAGE")
		if reference == "" {
			reference = "docker.io/library/busybox:1.36.1"
		}
		image, err := store.Pull(ctx, chamberImage.PullRequest{Reference: reference})
		if err != nil {
			t.Fatal(err)
		}
		rt, err := chamberRuntimeFactory.NewRuntime(ctx, chamberRuntime.DefaultConfig(root))
		if err != nil {
			t.Fatal(err)
		}
		container, err := rt.RunImage(ctx, image, chamberRuntime.RunImageRequest{
			ContainerID: "parent-death",
			Process:     chamberBundle.ProcessSpec{Args: []string{"/bin/sh", "-c", "trap 'exit 0' TERM; echo ready; while :; do sleep 1; done"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Publish only after the workload installs its TERM handler.
		testutil.Eventually(t, 10*time.Second, func() bool {
			data, _ := container.ReadLog(chamberRuntime.StdoutLogStream)
			return strings.Contains(string(data), "ready")
		})
		if err := os.WriteFile(filepath.Join(root, "ready"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = container.Wait(ctx)
		t.Fatalf("workload unexpectedly finished before parent death: %v", err)
	}
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(root, "helper.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	child := subprocess.CommandContext(ctx, executable, "-test.run=^TestRuntimeParentDeathIntegration$")
	child.Env = append(os.Environ(), "CHAMBER_CRASH_ROOT="+root)
	child.Stdout, child.Stderr = output, output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	t.Cleanup(func() { _ = child.Process.Kill() })
	defer func() {
		if t.Failed() {
			data, _ := os.ReadFile(output.Name())
			t.Logf("helper output:\n%s", data)
		}
	}()
	testutil.Eventually(t, 2*time.Minute, func() bool {
		select {
		case err := <-done:
			t.Fatalf("runtime helper exited before readiness: %v", err)
		default:
		}
		_, err := os.Stat(filepath.Join(root, "ready"))
		return err == nil
	})
	config := chamberRuntime.DefaultConfig(root)
	statePath := filepath.Join(config.RuntimeRoot, "parent-death", "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		InitPID int `json:"init_process_pid"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.InitPID <= 0 {
		t.Fatalf("state=%s, error=%v", data, err)
	}
	runcPIDs := testutil.LiveProcessesMatching(config.RuntimeRoot)
	if len(runcPIDs) == 0 {
		t.Fatal("no live runc process found")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	testutil.Eventually(t, 15*time.Second, func() bool {
		for _, pid := range append(runcPIDs, state.InitPID) {
			if testutil.ProcessAlive(pid) {
				return false
			}
		}
		return true
	})
	bundles, err := filepath.Glob(filepath.Join(config.RuntimeRoot, "bundles", "image-*"))
	if err != nil || len(bundles) != 1 {
		t.Fatalf("crash retained bundles=%v, error=%v", bundles, err)
	}
	stdout := filepath.Join(config.RuntimeRoot, "logs", "parent-death", "stdout.log")
	if data, err := os.ReadFile(stdout); err != nil || !strings.Contains(string(data), "ready") {
		t.Fatalf("retained log=%q, error=%v", data, err)
	}
	// Crash cleanup remains an explicit caller responsibility. The private root
	// contains the bundle, logs, and caches and is removed by t.TempDir afterward.
	t.Logf("parent SIGKILL: runc=%v init=%s stopped; bundle and logs retained at %s", runcPIDs, strconv.Itoa(state.InitPID), root)
	if _, err := os.Stat(filepath.Join(bundles[0], "config.json")); err != nil {
		t.Fatal(fmt.Errorf("retained bundle: %w", err))
	}
}
