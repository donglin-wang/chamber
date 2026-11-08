# Chamber

Chamber is a Go SDK for rootless OCI container execution on Linux. Embed container
workloads in Go applications, automation, and CI pipelines: pull or build an image,
run it with `Runtime.RunImage`, and wait for the result. Chamber manages image
storage, bundle provisioning, and runtime tools under caller-selected roots.

## Setup

Use Go 1.26.4 or newer and a Linux host or VM with rootless container support.
From a repository checkout, configure a dedicated account and switch to it:

```sh
sudo ./scripts/setup-host.sh chamber
sudo -iu chamber
```

Setup installs UID/GID mapping helpers, allocates subordinate IDs, and enables
user namespaces. On Ubuntu with restricted user namespaces, add
`--allow-unrestricted-userns` before `chamber` in the setup command; this disables
the system-wide AppArmor user-namespace restrictions.

As `chamber`, with Go on your `PATH`, create a temporary directory and module,
then add Chamber. Image pulls and managed tool downloads need network access.

```sh
export TMPDIR="$HOME/tmp"
mkdir -p "$TMPDIR"
mkdir chamber-demo
cd chamber-demo
go mod init example.com/chamber-demo
go get github.com/donglin-wang/chamber
```

## Example

Save this as `main.go` and run `go run .`. It pulls Alpine, prints
`hello from chamber` and `exit=0`, then deletes the container and temporary files.

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

	runc, err := chamberRuntimeFactory.NewRuntime(ctx, chamberRuntime.DefaultConfig(root))
	if err != nil {
		panic(err)
	}

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

Host logs use `msg` for the event message or subprocess output line and `source`
for the implementation: `image/store`, `image/buildkit`, `bundle/directory`, or
`runtime/runc`. BuildKit output also includes `process` and `stream`. Image and
runtime binary origins use `image_source` and `binary_source` respectively.

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and testing.
