package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
	"github.com/donglin-wang/chamber/pkg/shared/subprocess"
)

// A process group prevents Git credential helpers and build tools outliving cancellation.
func runCommand(ctx context.Context, dir string, output io.Writer, name string, args ...string) error {
	command := subprocess.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = commandEnvironment()
	command.Stdout, command.Stderr = output, output
	subprocess.SetProcessGroup(command)
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 5 * time.Second
	return command.Run()
}

// Tokens and the update passcode are never passed to checkout/build subprocesses.
// Git's normal credential helpers and SSH agent remain available for private repos.
func commandEnvironment() []string {
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GOWORK=off", "CGO_ENABLED=0"}
	for _, key := range []string{"PATH", "HOME", "USER", "TMPDIR", "SSH_AUTH_SOCK", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func checkout(ctx context.Context, destination, cloneURL, revision string, output io.Writer) error {
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "--quiet", destination},
		{"-C", destination, "remote", "add", "origin", cloneURL},
		{"-C", destination, "fetch", "--quiet", "--depth=1", "origin", revision},
		{"-C", destination, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if err := runCommand(ctx, "", output, "git", args...); err != nil {
			return fmt.Errorf("checkout: %w", err)
		}
	}
	return nil
}

func runtimeConfig(c config, work string, output io.Writer) chamberRuntime.Config {
	r := chamberRuntime.DefaultConfig(work)
	r.RuntimePath = c.RuntimePath
	r.RuntimeTmpRoot = filepath.Join(work, "tmp", "runtime")
	r.RuntimeBinTmpRoot = filepath.Join(work, "tmp", "runtime-bin")
	r.Logging = chamberLogging.Config{Logger: slog.New(slog.NewTextHandler(output, nil))}
	return r
}

// Keep socket paths short regardless of the durable state's location. The
// durable root participates in the key so independent CI installations do not
// share a workspace even when testing the same commit.
func jobWorkspace(c config, id string) string {
	root := c.WorkRoot
	if root == "" {
		root = fmt.Sprintf("/tmp/chamber-ci-%d", os.Geteuid())
	}
	key := sha256.Sum256([]byte(c.Root + "\x00" + id))
	return filepath.Join(root, fmt.Sprintf("%x", key[:12]))
}

func executeJob(ctx context.Context, c config, j job, output io.Writer) (state string, err error) {
	work := jobWorkspace(c, j.ID)
	source := filepath.Join(work, "source")
	fmt.Fprintf(output, "Checking out %s at %s\n", c.Repositories[j.Repository].Name, j.SHA)
	if err := checkout(ctx, source, c.Repositories[j.Repository].CloneURL, j.SHA, output); err != nil {
		// Some servers only permit a reachable ref; verify its exact SHA before use.
		if ctx.Err() != nil {
			return "error", ctx.Err()
		}
		if err := runCommand(ctx, "", output, "git", "-C", source, "fetch", "--quiet", "--depth=1", "origin", j.Ref); err != nil {
			return "error", err
		}
		if err := runCommand(ctx, "", output, "git", "-C", source, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
			return "error", err
		}
	}
	var head strings.Builder
	if err := runCommand(ctx, "", &head, "git", "-C", source, "rev-parse", "HEAD"); err != nil {
		return "error", err
	}
	if strings.TrimSpace(head.String()) != j.SHA {
		return "error", fmt.Errorf("fetched commit does not match webhook SHA")
	}
	dockerfile := filepath.Join(source, "Dockerfile.chamber")
	info, err := os.Lstat(dockerfile)
	if err != nil || !info.Mode().IsRegular() {
		return "failure", fmt.Errorf("repository root must contain a regular Dockerfile.chamber")
	}
	imageConfig := chamberImage.DefaultConfig(work)
	imageConfig.TmpRoot = filepath.Join(work, "i")
	imageConfig.BuildKit = c.BuildKit
	imageConfig.Logging = chamberLogging.Config{Logger: slog.New(slog.NewTextHandler(output, nil))}
	store, err := chamberImageFactory.NewStore(imageConfig)
	if err != nil {
		return "error", err
	}
	fmt.Fprintln(output, "Building Dockerfile.chamber")
	image, err := store.Build(ctx, chamberImage.BuildRequest{Reference: "chamber.local/ci:" + j.ID, ContextPath: source, DockerfilePath: dockerfile})
	if err != nil {
		return "failure", err
	}
	rt, err := chamberRuntimeFactory.NewRuntime(ctx, runtimeConfig(c, work, output))
	if err != nil {
		return "error", err
	}
	fmt.Fprintln(output, "Running image")
	container, err := rt.RunImage(ctx, image, chamberRuntime.RunImageRequest{ContainerID: j.ID, Stdout: []io.Writer{output}, Stderr: []io.Writer{output}})
	if err != nil {
		return "error", err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if cleanupErr := container.Delete(cleanupCtx, true); cleanupErr != nil {
			state, err = "error", errors.Join(err, fmt.Errorf("delete container: %w", cleanupErr))
		}
	}()
	result, err := container.Wait(ctx)
	if err != nil {
		return "error", err
	}
	if result.Status != chamberRuntime.ContainerResultStatusExited || result.ExitCode == nil {
		return "error", fmt.Errorf("container returned no exit result")
	}
	fmt.Fprintf(output, "Container exited with code %d\n", *result.ExitCode)
	if *result.ExitCode != 0 {
		return "failure", fmt.Errorf("container exited with code %d", *result.ExitCode)
	}
	return "success", nil
}

// Reopened handles cannot remove RunImage bundles. First delete the runc state,
// then remove this application's entire private per-job workspace.
func cleanupJob(ctx context.Context, c config, j job) error {
	work := jobWorkspace(c, j.ID)
	statePath := filepath.Join(work, "run", "runtime", j.ID)
	if _, err := os.Stat(statePath); err == nil {
		rt, err := chamberRuntimeFactory.NewRuntime(ctx, runtimeConfig(c, work, io.Discard))
		if err != nil {
			return err
		}
		handle, err := rt.Open(ctx, j.ID)
		if err != nil {
			return err
		}
		if err := handle.Delete(ctx, true); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(work)
}
