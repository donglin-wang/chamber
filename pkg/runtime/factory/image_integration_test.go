package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

func TestRunImageIntegration(t *testing.T) {
	if os.Getenv("CHAMBER_INTEGRATION") != "1" {
		t.Skip("set CHAMBER_INTEGRATION=1 to run real rootless containers on Linux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := chamberImageFactory.NewStore(chamberImage.DefaultConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.Pull(ctx, chamberImage.PullRequest{Reference: "docker.io/library/busybox:1.36.1"})
	if err != nil {
		t.Fatal(err)
	}
	config := chamberRuntime.DefaultConfig(root)
	rt, err := chamberRuntimeFactory.NewRuntime(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	mounted := filepath.Join(root, "input")
	if err := os.WriteFile(mounted, []byte("mounted input"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("process mounts logs and cleanup", func(t *testing.T) {
		container, err := rt.RunImage(ctx, image, chamberRuntime.RunImageRequest{
			ContainerID: "integration-image",
			Process: chamberBundle.ProcessSpec{
				Args: []string{"/bin/sh", "-c", "cat /input; echo error-output >&2; exit 7"},
			},
			Mounts: []chamberBundle.Mount{{Source: mounted, Target: "/input"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := container.Delete(context.Background(), true); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		})
		result, err := container.Wait(ctx)
		if err != nil || result.ExitCode == nil || *result.ExitCode != 7 {
			stderr, _ := container.ReadLog(chamberRuntime.StderrLogStream)
			t.Fatalf("Wait = %#v, %v; want exit 7; stderr=%s", result, err, stderr)
		}
		bundles, err := filepath.Glob(filepath.Join(config.RuntimeRoot, "bundles", "image-*"))
		if err != nil || len(bundles) != 1 {
			t.Fatalf("bundle ownership before Delete: entries=%v, err=%v", bundles, err)
		}
		if err := container.Delete(ctx, true); err != nil {
			t.Fatal(err)
		}
		for stream, want := range map[chamberRuntime.LogStream]string{
			chamberRuntime.StdoutLogStream: "mounted input",
			chamberRuntime.StderrLogStream: "error-output",
		} {
			content, err := container.ReadLog(stream)
			if err != nil || !strings.Contains(string(content), want) {
				t.Fatalf("ReadLog(%s) = %q, %v; want %q", stream, content, err, want)
			}
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		container, err := rt.RunImage(ctx, image, chamberRuntime.RunImageRequest{
			Process: chamberBundle.ProcessSpec{Args: []string{"/bin/sh", "-c", "echo ready; exec sleep 60"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := container.Delete(context.Background(), true); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		})
		waitCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		_, err = container.Wait(waitCtx)
		if !errors.Is(err, chamberErrors.ErrCanceled) {
			stderr, _ := container.ReadLog(chamberRuntime.StderrLogStream)
			t.Fatalf("Wait = %v, want canceled; stderr=%s", err, stderr)
		}
	})

	for _, relative := range []string{"bundles", "tmp/bundles"} {
		entries, err := filepath.Glob(filepath.Join(config.RuntimeRoot, relative, "*image-*"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("leftover resources in %s: entries=%v, err=%v", relative, entries, err)
		}
	}
	if err := chamberImage.ValidateLayout(image.LayoutPath); err != nil {
		t.Fatalf("shared image changed during cleanup: %v", err)
	}
}
