package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
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
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if container != nil {
			if cleanupErr := container.Delete(cleanupCtx, true); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup failed; state retained at %s: %w", root, cleanupErr))
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
	container, err = rt.RunImage(ctx, image, chamberRuntime.RunImageRequest{
		ContainerID: "local-test",
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
