package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberBundleFactory "github.com/donglin-wang/chamber/pkg/bundle/factory"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code, err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func run(ctx context.Context) (code int, err error) {
	project, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	root, err := os.MkdirTemp("", "chamber-local-test-")
	if err != nil {
		return 1, err
	}
	var container chamberRuntime.Container
	var provisioner chamberBundle.Provisioner
	var bundle chamberBundle.ProvisionedBundle
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if container != nil {
			if cleanupErr := container.Delete(cleanupCtx, true); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup failed; state retained at %s: %w", root, cleanupErr))
				return
			}
		}
		// Run accepts a caller-owned bundle, so stop the container before removing it.
		if bundle.BundleID != "" {
			if cleanupErr := provisioner.Remove(cleanupCtx, bundle); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("bundle cleanup failed; state retained at %s: %w", root, cleanupErr))
				return
			}
		}
		err = errors.Join(err, os.RemoveAll(root))
	}()

	store, err := chamberImageFactory.NewStore(chamberImage.DefaultConfig(root))
	if err != nil {
		return 1, err
	}
	image, err := store.Build(ctx, chamberImage.BuildRequest{
		Reference:      "chamber.local/local-test:latest",
		ContextPath:    project,
		DockerfilePath: filepath.Join(project, "Dockerfile.chamber"),
	})
	if err != nil {
		return 1, err
	}
	rt, err := chamberRuntimeFactory.NewRuntime(ctx, chamberRuntime.DefaultConfig(root))
	if err != nil {
		return 1, err
	}
	bundleConfig := chamberBundle.DefaultConfig(root)
	bundleConfig.TmpRoot = filepath.Join(root, "tmp", "bundles")
	provisioner, err = chamberBundleFactory.NewProvisioner(bundleConfig)
	if err != nil {
		return 1, err
	}
	terminal := false
	bundle, err = provisioner.Provision(ctx, chamberBundle.ProvisionRequest{
		BundleID:      "local-test",
		ImageLayout:   image.LayoutPath,
		ImageRef:      image.Reference,
		ImageDigest:   image.Digest,
		ImagePlatform: image.Platform,
		Process:       chamberBundle.ProcessSpec{Terminal: &terminal},
	})
	if err != nil {
		return 1, err
	}
	if err := configureNestedTestContainer(bundle.BundlePath); err != nil {
		return 1, err
	}
	container, err = rt.Run(ctx, chamberRuntime.RunRequest{
		ContainerID: "local-test",
		BundlePath:  bundle.BundlePath,
		Stdout:      []io.Writer{os.Stdout},
		Stderr:      []io.Writer{os.Stderr},
	})
	if err != nil {
		return 1, err
	}
	result, err := container.Wait(ctx)
	if err != nil {
		return 1, err
	}
	if result.Status != chamberRuntime.ContainerResultStatusExited || result.ExitCode == nil {
		return 1, fmt.Errorf("container did not report an exit code: %s", result.Status)
	}
	return *result.ExitCode, nil
}

// This caller-owned bundle permits mapping helpers and nested proc mounts.
// The test process gains no additional effective or ambient capabilities.
func configureNestedTestContainer(bundlePath string) error {
	path := filepath.Join(bundlePath, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read test container spec: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("decode test container spec: %w", err)
	}
	if spec.Process == nil || spec.Process.Capabilities == nil || spec.Linux == nil {
		return fmt.Errorf("test container spec requires a process, capability bounds and Linux settings")
	}
	spec.Process.NoNewPrivileges = false
	for _, capability := range []string{"CAP_SETUID", "CAP_SETGID"} {
		if !slices.Contains(spec.Process.Capabilities.Bounding, capability) {
			spec.Process.Capabilities.Bounding = append(spec.Process.Capabilities.Bounding, capability)
		}
	}
	// Locked proc submounts prevent nested runc and BuildKit workers from
	// mounting proc for their own PID namespaces. Keep other path restrictions.
	isProcPath := func(path string) bool {
		return path == "/proc" || strings.HasPrefix(path, "/proc/")
	}
	spec.Linux.MaskedPaths = slices.DeleteFunc(spec.Linux.MaskedPaths, isProcPath)
	spec.Linux.ReadonlyPaths = slices.DeleteFunc(spec.Linux.ReadonlyPaths, isProcPath)
	data, err = json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode test container spec: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write test container spec: %w", err)
	}
	return nil
}
