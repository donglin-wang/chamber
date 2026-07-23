# Chamber

Chamber is a Go SDK for point-and-shoot OCI container execution. The
SDK under `pkg/` can pull an image into a caller-owned root, provision an OCI
runtime bundle, run that bundle with `runc`, and read runtime logs.
These primitives are intended to be a foundation for derived products such as a
container daemon, workflow orchestration, and container orchestration layers.

**The current repo is experimental and interfaces are volatile.**

The daemon code in this repository is still experimental and is not part of the
SDK contract.

## SDK Scope

- `pkg/image`: creates image pullers with `image.NewPuller` and stores pulled
  images as OCI image layouts under a caller-provided root.
- `pkg/bundle`: creates bundle provisioners with `bundle.NewProvisioner`. The
  current internal implementation, `directory`, unpacks an OCI layout into a
  rootless OCI runtime bundle.
- `pkg/runtime`: creates runtimes with `runtime.NewRuntime`. The current
  internal implementation, `runc`, downloads or reuses a pinned `runc` binary
  and runs provisioned bundles.
- `pkg/shared`: common error codes, filesystem policy, logging, image reference
  validation, container ID validation, and capability vocabulary.

SDK callers own storage placement, concurrency, cleanup, cancellation policy,
and recovery. Constructors return ready objects or errors; there is no separate
public setup phase.

## Concurrency Warning

The SDK is not currently focused on thread-safe shared use. It does not provide
automatic locking, operation records, leases, or cross-process coordination for
shared roots and container IDs.

Callers that use Chamber from multiple goroutines or processes must provide
their own concurrency management. In practice, that means serializing or locking
access to shared image roots, bundle roots, runtime roots, container IDs, log
paths, cleanup, and cancellation decisions.

## Experimental Daemon

The `daemon/` package contains the current `chamberd` prototype. It is the first
in-repo composition layer built from the public SDK packages rather than a
separate container engine.

Today the daemon:

- loads daemon config from defaults, a JSON config file, and command-line
  overrides;
- composes the image puller, bundle provisioner, runtime, and metadata store;
- exposes an HTTP API for health checks, OpenAPI docs, image pull, container
  run, container list, and stored container logs;
- records image, operation, and container metadata under daemon-owned storage;
- provides `chamberd storage remove --yes` for deleting the derived Chamber
  storage root.

The daemon is still a prototype. It shows how a local authority can own
metadata, operation state, runtime composition, and API responses, but it should
not yet be treated as a stable production daemon with complete recovery,
lease-aware garbage collection, cancellation, or multi-client coordination.

## Cleanup Contract

The SDK does not provide all-in-one container cleanup. Callers are responsible
for cleaning up the storage they asked each package to create.

For one container run, callers should:

1. Call `Container.Wait` for every successful `Runtime.Run` call. This reaps the
   `runc run` process and closes Chamber-owned stdout/stderr log file handles.
2. Call `Container.Delete` with `force` set to `true` when runtime state may still exist
   or the container may still be running. This delegates to `runc delete
   --force`.
3. Remove `ProvisionedBundle.BundlePath` when the unpacked bundle is no longer
   needed.
4. Call `Container.DeleteLog` for default stdout/stderr logs when they are no
   longer needed, or remove `<RuntimeRoot>/logs/<containerID>` directly.
5. Decide separately when to remove shared image layouts and the cached runtime
   binary.

If a process crashes or a caller skips these steps, per-container runtime state,
bundle directories, logs, or temporary files may remain in the caller-provided
roots.

The context passed to `Runtime.Run` controls launch work only. After `Run`
returns a `Container`, that container owns lifecycle operations; use
`Container.Signal`, `Container.Delete`, and `Container.Wait` to stop, remove, and
observe it.

## Requirements

- Go 1.26.4 or newer compatible with this module.
- A Linux host or Linux VM for runtime execution.
- Rootless container support for the current `directory` bundle provisioner and
  `runc` runtime.
- Non-interactive processes. The current `runc` runtime does not allocate PTYs,
  so bundles with `process.terminal=true` are rejected. Set
  `ProcessSpec.Terminal` to `false` when running images that default to a
  terminal process.
- The current rootless bundle provisioner maps only container UID/GID `0` to the
  current host user and group. Images or `ProcessUser` overrides that require
  unmapped UIDs or GIDs are rejected during bundle provisioning.
- Network access when pulling images or when the pinned `runc` binary is not
  already present in the configured runtime binary directory.

## Minimal SDK Flow

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/donglin-wang/chamber/pkg/bundle"
	chamberBundleShared "github.com/donglin-wang/chamber/pkg/bundle/shared"
	"github.com/donglin-wang/chamber/pkg/image"
	chamberImageShared "github.com/donglin-wang/chamber/pkg/image/shared"
	"github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeShared "github.com/donglin-wang/chamber/pkg/runtime/shared"
	"github.com/donglin-wang/chamber/pkg/shared/capability"
	"github.com/donglin-wang/chamber/pkg/shared/localfs"
)

func main() {
	ctx := context.Background()
	root := "/tmp/chamber-sdk-demo"
	directoryManager := localfs.NewDirectoryManager()

	puller, err := image.NewPuller(chamberImageShared.Config{
		Root: filepath.Join(root, "images"),
	}, directoryManager)
	if err != nil {
		panic(err)
	}
	pulled, err := puller.Pull(ctx, chamberImageShared.PullRequest{
		Reference: "docker.io/library/alpine:latest",
	})
	if err != nil {
		panic(err)
	}

	provisioner, err := bundle.NewProvisioner(chamberBundleShared.Config{
		Root:      filepath.Join(root, "bundles"),
		Name:      chamberBundleShared.ProvisionerNameDirectory,
		Privilege: capability.Rootless,
	}, directoryManager)
	if err != nil {
		panic(err)
	}
	terminal := false
	provisioned, err := provisioner.Provision(ctx, chamberBundleShared.ProvisionRequest{
		ContainerID: "demo",
		ImageLayout: pulled.LayoutPath,
		ImageRef:    pulled.Reference,
		Process: chamberBundleShared.ProcessSpec{
			Args:     []string{"/bin/sh", "-c", "echo hello from chamber"},
			Terminal: &terminal,
		},
	})
	if err != nil {
		panic(err)
	}

	runc, err := runtime.NewRuntime(ctx, chamberRuntimeShared.Config{
		RuntimeRoot:   filepath.Join(root, "run", "runtime"),
		RuntimeBinDir: filepath.Join(root, "bin"),
		Name:          chamberRuntimeShared.RuntimeNameRunc,
		Privilege:     capability.Rootless,
	}, directoryManager)
	if err != nil {
		panic(err)
	}
	container, err := runc.Run(ctx, chamberRuntimeShared.RunRequest{
		Bundle: provisioned,
		Stdout: []io.Writer{os.Stdout},
		Stderr: []io.Writer{os.Stderr},
	})
	if err != nil {
		panic(err)
	}
	defer func() {
		_ = container.Delete(context.Background(), true)
		_ = container.DeleteLog(chamberRuntimeShared.StdoutLogStream)
		_ = container.DeleteLog(chamberRuntimeShared.StderrLogStream)
		_ = os.RemoveAll(provisioned.BundlePath)
	}()
	result, err := container.Wait()
	if err != nil {
		panic(err)
	}
	stdout, err := container.ReadLog(chamberRuntimeShared.StdoutLogStream)
	if err != nil {
		panic(err)
	}
	fmt.Printf("exit=%d stdout=%s", result.ExitCode, stdout)
}
```

## Validation

Use an explicit Go cache in restricted macOS environments:

```sh
GOCACHE=/tmp/chamber-go-cache go test ./...
GOCACHE=/tmp/chamber-go-cache go vet ./...
```

The default test suite avoids real registry pulls. To include registry
integration coverage, opt in explicitly:

```sh
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/image/internal/puller -run TestImagePullerRealWorldBusybox
```
