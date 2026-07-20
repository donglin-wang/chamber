# Chamber

Chamber is an early Go SDK for point-and-shoot OCI container execution. The
0.1.0 beta scope is the SDK under `pkg/`: pull an image into a caller-owned
root, provision an OCI runtime bundle, run that bundle with `runc`, and read
runtime logs.

The daemon code in this repository is still experimental and is not part of the
0.1.0 beta contract.

## SDK Beta Scope

- `pkg/image`: creates image pullers and stores pulled images as OCI image
  layouts under a caller-provided root.
- `pkg/bundle`: creates bundle provisioners. The current implementation,
  `directory`, unpacks an OCI layout into a rootless OCI runtime bundle.
- `pkg/runtime`: creates runtimes. The current implementation, `runc`, downloads
  or reuses a pinned `runc` binary and runs provisioned bundles.
- `pkg/shared`: common error codes, filesystem policy, logging, image reference
  validation, container ID validation, and capability vocabulary.

SDK callers own storage placement, concurrency, cleanup, cancellation policy,
and recovery. Constructors return ready objects or errors; there is no separate
public setup phase.

## Cleanup Contract

The SDK does not provide all-in-one container cleanup in the 0.1.0 beta.
Callers are responsible for cleaning up the storage they asked each package to
create.

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

## Requirements

- Go 1.26.4 or newer compatible with this module.
- A Linux host or Linux VM for runtime execution.
- Rootless container support for the current `directory` bundle provisioner and
  `runc` runtime.
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
	provisioned, err := provisioner.Provision(ctx, chamberBundleShared.ProvisionRequest{
		ContainerID: "demo",
		ImageLayout: pulled.LayoutPath,
		ImageRef:    pulled.Reference,
		Process: chamberBundleShared.ProcessSpec{
			Args: []string{"/bin/sh", "-c", "echo hello from chamber"},
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
