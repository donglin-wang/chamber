# Chamber

Chamber is a Go SDK for point-and-shoot OCI container execution. The
SDK under `pkg/` can pull an image into a caller-owned root, provision an OCI
runtime bundle, run that bundle with `runc`, and read runtime logs.
Use it to embed container execution in Go applications, automation, and CI
pipelines. Pull or build an image, pass it to `Runtime.RunImage`, and wait for
the result. The runtime manages bundle provisioning and its workspace.

**The current repo is experimental and interfaces are volatile.**

## SDK Scope

- `pkg/image`: public image contracts and config; `pkg/image/factory` creates
  image stores for pulling and building images under a caller-provided root.
  Returned images carry their local layout path, digest, and platform.
- `pkg/bundle`: public bundle contracts and config; `pkg/bundle/factory`
  creates bundle provisioners. The current internal implementation, `directory`,
  unpacks an OCI layout into a rootless OCI runtime bundle.
- `pkg/runtime`: public runtime contracts and config; `pkg/runtime/factory`
  creates runtimes. The current internal implementation, `runc`, downloads or
  reuses a pinned `runc` binary. `RunImage` provisions and runs a local image;
  `Run` accepts a ready bundle for callers that need manual provisioning.
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

## Cleanup Contract

For each successful `RunImage` call:

1. Call `Container.Wait` to observe completion. This waits for the `runc run`
   process and closes Chamber-owned stdout/stderr log file handles.
2. Call `Container.Delete(ctx, true)` to terminate any remaining container,
   remove runtime state, and remove its provisioned bundle. This is also safe
   after `Wait` cancels or `runc` has already removed its state.
3. Call `Container.DeleteLog` for each stream when logs are no longer needed.

`RunImage` creates private bundles under `<RuntimeRoot>/bundles`, with staging
under `<RuntimeRoot>/tmp/bundles`. Launch failures remove the provisioned bundle
before returning an error. Cleanup failures are returned so callers can handle
them. Keep the image's layout available until provisioning finishes.

`Runtime.Run` remains available for ready OCI bundles. Those bundles remain
caller-owned: use `Provisioner.Remove` after the container is deleted.

Shared images and cached binaries remain until the caller removes them. Callers
also own cleanup after a process crash; there is no automatic crash recovery.

The contexts passed to `RunImage` and `Run` control provisioning/launch work
only. After either returns a `Container`, use its lifecycle methods to control
execution. `Wait` with a canceled context attempts to force-delete the container;
call `Delete` afterward to complete cleanup and handle any errors.

## Requirements

- Go 1.26.4 or newer compatible with this module.
- A Linux host or Linux VM for runtime execution.
- Rootless container support for the current `directory` bundle provisioner and
  `runc` runtime.
- Non-interactive processes. The current `runc` runtime does not allocate PTYs,
  so bundles with `process.terminal=true` are rejected. Set
  `ProcessSpec.Terminal` to `false` when provisioning images for `Run`.
  `RunImage` defaults to a non-terminal process.
- The current rootless bundle provisioner maps only container UID/GID `0` to the
  current host user and group. Images or `ProcessUser` overrides that require
  unmapped UIDs or GIDs are rejected during bundle provisioning.
- Network access when pulling images or when the pinned `runc` binary is not
  already present in the configured runtime binary directory.

## Quick Start: Pull and Run

On a rootless-capable Linux host, save this as `main.go` in a Go module that
depends on Chamber and run `go run .`. It pulls Alpine and runs `/bin/echo`
inside the container, printing `hello from chamber` and `exit=0`.

The example uses a private temporary root and removes its image, bundle, runtime
binary, and logs when it finishes. Cleanup errors are ignored here for brevity;
applications should handle them according to their cleanup policy.

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	chamberBundle "github.com/donglin-wang/chamber/pkg/bundle"
	chamberImage "github.com/donglin-wang/chamber/pkg/image"
	chamberImageFactory "github.com/donglin-wang/chamber/pkg/image/factory"
	chamberRuntime "github.com/donglin-wang/chamber/pkg/runtime"
	chamberRuntimeFactory "github.com/donglin-wang/chamber/pkg/runtime/factory"
)

func main() {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "chamber-sdk-demo-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	// 1. Pull an image into the image store.
	imageStore, err := chamberImageFactory.NewStore(chamberImage.DefaultConfig(root))
	if err != nil {
		panic(err)
	}
	image, err := imageStore.Pull(ctx, chamberImage.PullRequest{
		Reference: "docker.io/library/alpine:latest",
	})
	if err != nil {
		panic(err)
	}

	// 2. Create a runtime; it owns the provisioner and bundle workspace.
	runc, err := chamberRuntimeFactory.NewRuntime(ctx, chamberRuntime.DefaultConfig(root))
	if err != nil {
		panic(err)
	}
	// 3. Run the image, stream output, and wait for it to exit.
	container, err := runc.RunImage(ctx, image, chamberRuntime.RunImageRequest{
		Process: chamberBundle.ProcessSpec{
			Args: []string{"/bin/echo", "hello from chamber"},
		},
		Stdout: []io.Writer{os.Stdout},
		Stderr: []io.Writer{os.Stderr},
	})
	if err != nil {
		panic(err)
	}
	defer func() {
		_ = container.Delete(context.Background(), true)
		_ = container.DeleteLog(chamberRuntime.StdoutLogStream)
		_ = container.DeleteLog(chamberRuntime.StderrLogStream)
	}()
	result, err := container.Wait(ctx)
	if err != nil {
		panic(err)
	}
	if result.ExitCode != nil {
		fmt.Printf("exit=%d\n", *result.ExitCode)
	}
}
```

`image.Store.Build` uses an ephemeral rootless BuildKit server for Dockerfile
builds. Set `image.BuildKit.BuildctlPath`, `BuildkitdPath`,
`RootlessKitPath`, and `RuncPath` to use local tool binaries. When paths are
empty, Chamber downloads and caches managed BuildKit, RootlessKit, and runc
binaries below `<image-root>/bin`. Per-build server state, runtime directories,
home/config directories, temporary files, and the OCI output archive stay below
the configured image `TmpRoot` and are removed after the build.

## Validation

Run tests and vet on a rootless-capable Linux host or inside a Linux VM such as
Lima. Native macOS is a development host, not a supported execution target.

```sh
GOCACHE=/tmp/chamber-go-cache go test ./...
GOCACHE=/tmp/chamber-go-cache go vet ./...
```

The default test suite avoids real registry pulls and real Dockerfile builds. To
include integration coverage, opt in explicitly on a rootless-capable Linux
host:

```sh
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/runtime/factory -run TestRunImageIntegration
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/image/internal/store -run TestStoreRealWorldBusybox
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/image/internal/store -run TestStoreRealBuildKit
```

The optional dogfood test runs this repository's tests inside a container using
`RunImage` with the checkout mounted at `/workspace`:

```sh
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./cmd/github-ci/pipeline -run TestRunDogfoodIntegration
```

`cmd/github-ci` provides a GitHub webhook CI example built on the same SDK.
It creates a separate root per admitted run; admission limits and retention
belong to this application. The SDK does not manage shared-root concurrency.
