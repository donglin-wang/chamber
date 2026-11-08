# Chamber

Chamber is a Go SDK for point-and-shoot OCI container execution. The SDK under `pkg/` can pull an image into a caller-owned root, provision an OCI runtime bundle, run that bundle with `runc`, and read runtime logs. Use it to embed container execution in Go applications, automation, and CI pipelines. Pull or build an image, pass it to `Runtime.RunImage`, and wait for the result. The runtime manages bundle provisioning and its workspace.

**The current repo is experimental and interfaces are volatile.**

See the [compiler example](examples/compile/README.md) for a complete application,
[host setup](scripts/README.md) for distro preparation, and the
[architecture](docs/architecture.md) and [measurements](docs/performance.md) for
ownership, lifecycle evidence, and reproducible performance results.

## SDK Scope

- `pkg/image`: public image contracts and config; `pkg/image/factory` creates image stores for pulling and building images under a caller-provided root. Returned images carry their local layout path, digest, and platform.
- `pkg/bundle`: public bundle contracts and config; `pkg/bundle/factory` creates bundle provisioners. The current internal implementation, `directory`, unpacks an OCI layout into a rootless OCI runtime bundle.
- `pkg/runtime`: public runtime contracts and config; `pkg/runtime/factory` creates runtimes. The current internal implementation, `runc`, downloads or reuses a pinned `runc` binary. `RunImage` provisions and runs a local image; `Run` accepts a ready bundle for callers that need manual provisioning.
- `pkg/shared`: common error codes, filesystem policy, logging, image reference validation, container ID validation, and capability vocabulary.

SDK callers own storage placement, concurrency, cleanup, cancellation policy, and recovery. Constructors return ready objects or errors; there is no separate public setup phase.

## Concurrency Warning

The SDK is not currently focused on thread-safe shared use. It does not provide automatic locking, operation records, leases, or cross-process coordination for shared roots and container IDs.

Callers that use Chamber from multiple goroutines or processes must provide their own concurrency management. In practice, that means serializing or locking access to shared image roots, bundle roots, runtime roots, container IDs, log paths, cleanup, and cancellation decisions.

## Cleanup Contract

For each successful `RunImage` call:

1. Call `Container.Wait` to observe completion. This waits for the `runc run` process and closes Chamber-owned stdout/stderr log file handles.
2. Call `Container.Delete(ctx, true)` to terminate any remaining container, remove runtime state, and remove its provisioned bundle. This is also safe after `Wait` cancels or `runc` has already removed its state.
3. Call `Container.DeleteLog` for each stream when logs are no longer needed.

`RunImage` creates private bundles under `<RuntimeRoot>/bundles`, with staging under `<RuntimeRoot>/tmp/bundles`. Launch failures remove the provisioned bundle before returning an error. Cleanup failures are returned so callers can handle them. Keep the image's layout available until provisioning finishes.

`Runtime.Run` remains available for ready OCI bundles. Those bundles remain caller-owned: use `Provisioner.Remove` after the container is deleted. Chamber provisioning permits nested rootless containers by default. `RunImage` uses those defaults automatically. `Run` executes its supplied spec unchanged; callers using externally prepared bundles own their nesting configuration.

Shared images and cached binaries remain until the caller removes them. Callers also own cleanup after a process crash; there is no automatic crash recovery.

The contexts passed to `RunImage` and `Run` control provisioning/launch work only. After either returns a `Container`, use its lifecycle methods to control execution. `Wait` with a canceled context attempts to force-delete the container; call `Delete` afterward to complete cleanup and handle any errors.

`Run` and `RunImage` wait for runc's OCI initialization acknowledgment, including
for workloads that exit immediately. The acknowledgment is not application
readiness or proof of a successful final kernel exec. `Wait` cancellation uses
a separate ten-second cleanup deadline; failures retain both cancellation and
cleanup errors. Result `ExitCode` and `ExitedAt` are optional and remain unset
when completion was not observed. `StartedAt` records initialization acknowledgment;
`ExitedAt` records observed runtime completion. See [lifecycle details](docs/architecture.md).

## Requirements

- Go 1.26.4 or newer compatible with this module.
- A Linux host or Linux VM for runtime execution.
- Rootless container support for the current `directory` bundle provisioner and `runc` runtime.
- Non-interactive processes. The current `runc` runtime does not allocate PTYs, so bundles with `process.terminal=true` are rejected. Set `ProcessSpec.Terminal` to `false` when provisioning images for `Run`. `RunImage` defaults to a non-terminal process.
- Non-root image users require subordinate UID/GID ranges for the current host user in `/etc/subuid` and `/etc/subgid`, plus `unshare`, `newuidmap`, and `newgidmap`. Chamber uses the first configured range automatically: container ID 0 maps to the caller, and IDs starting at 1 map to the subordinate range. Named image users, numeric users, primary groups, and `ProcessUser` UID/GID overrides use these mappings. Without subordinate ranges, only container UID/GID 0 is supported. Supplementary groups remain restricted by rootless runc.
- Network access when pulling images or downloading managed tools. Mapped bundle unpacking automatically installs a checksum-verified umoci helper alongside the provisioner's bundles; container deletion retains this cache.

## Quick Start: Pull and Run

On a rootless-capable Linux host, save this as `main.go` in a Go module that depends on Chamber and run `go run .`. It pulls Alpine and runs `/bin/echo` inside the container, printing `hello from chamber` and `exit=0`.

The example uses a private temporary root and removes its image, bundle, runtime binary, and logs when it finishes. Cleanup errors are ignored here for brevity; applications should handle them according to their cleanup policy.

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

`image.Store.Build` uses an ephemeral rootless BuildKit server for Dockerfile builds. Set `image.BuildKit.BuildctlPath`, `BuildkitdPath`, `RootlessKitPath`, and `RuncPath` to use local tool binaries. When paths are empty, Chamber downloads and caches managed BuildKit, RootlessKit, and runc binaries below `<image-root>/bin`. Per-build server state, runtime directories, home/config directories, temporary files, and the OCI output archive stay below the configured image `TmpRoot` and are removed after the build.

## Validation

Run tests and vet on a rootless-capable Linux host or inside a Linux VM such as Lima. Native macOS is a development host, not a supported execution target.

```sh
GOCACHE=/tmp/chamber-go-cache go test ./...
GOCACHE=/tmp/chamber-go-cache go vet ./...
```

The default test suite avoids real registry pulls and real Dockerfile builds. To include integration coverage, opt in explicitly on a rootless-capable Linux host:

```sh
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/runtime/factory -run TestRunImageIntegration
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/image/internal/store -run TestStoreRealWorldBusybox
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/image/internal/store -run TestStoreRealBuildKit
```

To run the package suite inside a container on that Linux host:

```sh
GOCACHE=/tmp/chamber-go-cache go run cmd/local-test/main.go
```

The local runner builds `Dockerfile.chamber` and calls `RunImage`. Chamber provisioning permits UID/GID mapping helpers with specific file capabilities within the outer container's user namespace. It clears no-new-privileges, adds `CAP_SETUID`/`CAP_SETGID` to the capability bounding set, and removes `/proc` masks and read-only path restrictions so nested containers can mount their own `/proc`. Effective and ambient capabilities stay unchanged. The image delegates 65,534 IDs starting at 2,000 to its `chamber` user, covering the BusyBox test image's UID/GID 65,534. The host allocation must expose IDs 2,000–67,533 through the outer container mapping. Images or processes requiring higher inner IDs need a larger allocation. `RunImage` owns the bundle; deleting the container removes it before the runner removes its temporary root.

## SourceHut

The manifests in [`.builds/`](.builds/) run `go run ./cmd/local-test` on
Debian Trixie, Fedora (`latest`), and Ubuntu 24.04 VMs. The runner builds
`Dockerfile.chamber` and executes the integration suite inside its Debian-based
container on each host distribution.

Each job installs Go, then uses [`.builds/test.sh`](.builds/test.sh) and the
existing [host setup](scripts/README.md) to create a separate unprivileged test
account. Host setup uses sudo and permits disabling AppArmor's user-namespace
restrictions on the disposable VM; SDK tests run as the test account. Temporary
files stay under that account's home so nested mapping helpers can retain file
capabilities. Go automatically selects a toolchain compatible with `go.mod`.

Push these files to `https://git.sr.ht/~donglin-wang/chamber` to submit builds
automatically. SourceHut selects the pushed revision for the repository in each
manifest's `sources` list. CI requires a SourceHut subscription; these builds
use public sources and do not require custom secrets.

## CircleCI

The `test-host` matrix runs directly on CircleCI's native Ubuntu 24.04 and 26.04
machine executors. It uses the restored [host setup](scripts/README.md), verifies
repeatable ID allocation, and runs SDK tests and the compiler example as a fresh
non-root account. These jobs do not launch an additional VM.

[`.circleci/config.yml`](.circleci/config.yml) builds `Dockerfile.chamber` and runs its default command, `go test -count=1 -v -timeout=20m ./pkg/...`, with `CHAMBER_INTEGRATION=1` set by the image. Test output appears in the CircleCI job log, and a failing test fails the job.

The job uses a Linux VM with Docker installed. It disables Ubuntu's AppArmor restrictions on unprivileged user namespaces for that disposable VM so Chamber's host preflight permits rootless execution. It runs the test container with `--privileged` so nested rootless runc and BuildKit can create namespaces and mount filesystems. Tests run as the image's non-root `chamber` user. This grants privileges to the disposable CI test environment; SDK callers still use rootless execution. See CircleCI's [Linux VM guide](https://circleci.com/docs/guides/execute/using-linuxvm/).

To run the same commands locally with a Linux Docker daemon:

```sh
docker build --progress=plain -f Dockerfile.chamber -t chamber-tests .
docker run --rm --privileged chamber-tests
```

### Set up CircleCI from scratch

1. Commit and push these repository changes, including `.circleci/config.yml`, to the branch you will use for setup. CircleCI needs the file on GitHub before it can run it. Merge the config into the default branch so future PR branches inherit it.
2. [Create a CircleCI account](https://circleci.com/signup/). For the GitHub App setup described here, sign up with an email address, verify it, complete MFA setup, and create a CircleCI organization. This can be an organization just for you. See the [signup guide](https://circleci.com/docs/guides/getting-started/first-steps/).
3. In CircleCI, choose **Create Project**, name it `chamber`, and name its pipeline `packages`. Connect **GitHub Cloud**, install/authorize the **CircleCI GitHub App** for the GitHub account that owns this repo, and grant it access to `chamber`. Select the Chamber repository as both the checkout source and config source, with config path `.circleci/config.yml`. Use the existing config rather than generating a starter. See the [project setup guide](https://circleci.com/docs/guides/getting-started/create-project/).
4. In **Project Settings → Project Setup**, edit the pipeline's GitHub trigger (or set it during project creation). Set **Run on** to **PR opened or pushed to, default branch and tag pushes**. Use Chamber as the event source. Replace the default **All pushes** trigger to avoid duplicate builds. This preset runs when a PR opens or reopens and whenever commits are pushed to an open PR from a branch in this repository, including draft PRs. It also builds default-branch pushes. The config has no tag filter, so tag pushes do not run the test job. Trigger selection is a CircleCI project setting; adding YAML alone does not configure it. See the [trigger options](https://circleci.com/docs/guides/orchestrate/github-trigger-event-options/).
5. Open a PR from a branch containing the config. Confirm that CircleCI runs `test-packages` and GitHub shows its result. Push another commit to the same PR and confirm a new run appears for that commit.
6. Optional: after the first run, open GitHub **Settings → Rules → Rulesets** (or branch protection), require status checks for your default branch, and select the CircleCI check that actually appeared on the PR. This makes passing tests a merge requirement.

This job pulls public images and tools and requires no custom secrets, registry push credentials, webhook server, or self-hosted runner. CircleCI provides the repository checkout through its GitHub App integration.

### Include pull requests from forks

CircleCI's GitHub App pipelines do not automatically build forked PRs. If "all PR pushes" includes contributors' forks, use a **GitHub OAuth** project instead, with the same `.circleci/config.yml`:

1. In CircleCI **User Settings → Account Integrations**, follow the link **Received instructions from the CircleCI Support team to authorize a GitHub OAuth app?** and authorize GitHub. This creates a `github/<owner>` CircleCI organization. See [organization setup](https://circleci.com/docs/guides/getting-started/create-an-organization/).
2. Select that organization, choose **Set up a project**, select Chamber, and use the existing `.circleci/config.yml` from your setup branch.
3. In that project's **Project Settings → Advanced**, enable **Only build pull requests** and **Build forked pull requests**. Leave **Pass secrets to builds from forked pull requests** off; the package tests do not need secrets.
4. Use this OAuth pipeline as the required GitHub check and disable any earlier GitHub App trigger to avoid duplicate runs. Verify both opening a forked PR and pushing another commit to it.

The fork setting applies to OAuth pipelines, not the GitHub App setup above. See CircleCI's [pull request build settings](https://circleci.com/docs/guides/integration/pull-request-build-settings/).
