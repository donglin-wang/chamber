# `pkg/machine` Implementation Plan

## Goal

Add a public `pkg/machine` package that provisions and operates a local Linux VM so Chamber commands can ask for a usable machine instead of requiring the user to manually run `limactl`.

The first consumer should be `cmd/ci`: from macOS, the dog-food CI command should be able to ensure a Lima VM exists, start it, and run the existing Linux-only Chamber CI flow inside that VM.

## Boundary

`pkg/machine` should own host-machine lifecycle, not container lifecycle.

It should know how to:

- define a machine spec;
- create or reuse a named VM;
- start, stop, inspect, and delete that VM;
- run a command inside the VM with a working directory, environment, stdin/stdout/stderr, and exit status;
- expose enough status for callers to decide whether a machine is missing, stopped, running, or broken.

It should not know how to:

- pull OCI images;
- provision OCI bundles;
- run containers through `runc`;
- manage daemon operations, leases, metadata, GC, or crash recovery;
- decide CI job structure.

That keeps the existing split clean:

- `pkg/machine`: make a Linux host available.
- `cmd/ci`: compose Chamber image, bundle, and runtime SDK packages into dog-food jobs.
- future `chamberd`: may use a machine package at host-development or demo boundaries, but daemon reliability policy remains daemon-owned.

## Package Shape

Start with one generic public machine contract:

```go
package machine

type Config struct {
	Root    string
	Name    string
	Spec    Spec
	Start   bool
	Logging logging.Config
}

type Machine interface {
	Descriptor() Descriptor
	Run(context.Context, RunRequest) (RunResult, error)
	Stop(context.Context) error
	Delete(context.Context) error
}
```

Add a first concrete adapter under `pkg/machine/lima`.

Use `lima` in the concrete package name because the implementation is an adapter around the Lima CLI and instance model. The public `pkg/machine` package remains provider-neutral, similar to `pkg/runtime` vs. `pkg/runtime/runc`.

Follow the existing SDK constructor convention:

```go
vm, err := lima.New(ctx, machine.Config{
	Root:  root,
	Name:  "chamber-ci",
	Spec:  spec,
	Start: true,
}, directoryManager)
```

`New` should validate inputs, prepare private directories, render provider config, create or reuse the VM, and optionally start it. Do not add a daemon-shaped manager or ensure layer for this first SDK surface.

## Core Types

The public package should start small and explicit:

```go
type Spec struct {
	OS          string
	Arch        string
	CPUs        int
	MemoryBytes int64
	DiskBytes   int64
	Mounts      []Mount
	SetupScript string
}

type Mount struct {
	Source   string
	Target   string
	Writable bool
}

type RunRequest struct {
	Args   []string
	Workdir string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type RunResult struct {
	ExitCode int
}

type Descriptor struct {
	Name      string
	Status    Status
	Provider  string
	Directory string
	OS        string
	Arch      string
	CPUs      int
	MemoryBytes int64
	DiskBytes   int64
}
```

`RunRequest` is the typed SDK equivalent of `limactl shell --workdir <dir> <name> <args...>`: it runs a normal process inside the VM. `RunResult` belongs directly to `Machine.Run`, not to a separate handle object.

Keep provider-specific knobs in `pkg/machine/lima.Config` or adapter options, not in the generic contract, until they prove portable. Examples: `limactl` path, Lima home, VM type, mount type, Rosetta, `--containerd`, and create/start timeouts.

## First Machine Spec

Create a Chamber CI machine spec equivalent to the current `lima-config.yaml`:

- Ubuntu 24.04 cloud images for `aarch64` and `x86_64`;
- writable mount for the workspace;
- system provisioning script that enables unprivileged user namespaces and relaxes Ubuntu AppArmor user-namespace restrictions when that sysctl exists;
- no dependency on containerd for Chamber itself.

The public `machine.Spec` has one `SetupScript` because Chamber only needs one setup hook in the provider-neutral contract today. `pkg/machine/lima` can render that single script into Lima's list-shaped `provision` YAML internally.

Do not put this CI-specific spec directly into `pkg/machine`. Good homes:

- `cmd/ci` local helper for the first integration; or
- later `pkg/machine/presets` only if more callers need it.

## Lima Adapter Plan

Implement `pkg/machine/lima` around `limactl` first.

1. Add a small command runner abstraction so tests can use a fake `limactl`.
2. Validate input names and paths before invoking Lima.
3. Render a Lima YAML file from `machine.Spec` into the Chamber machine root.
4. Inside `lima.New`, run `limactl create --name=<name> <spec.yaml> --tty=false` when the instance does not exist.
5. Inside `lima.New`, run `limactl start <name> --timeout=<duration> --tty=false` when `Config.Start` is true and the instance is not running.
6. Inspect with `limactl list --format json <name>` and map Lima states into Chamber `machine.Status`.
7. Implement `Machine.Run` with `limactl shell --workdir <path> <name> -- <args...>`.
8. Implement `Machine.Stop` with `limactl stop <name>`.
9. Implement `Machine.Delete` with `limactl delete --force <name> --tty=false`.

Lima state isolation is viable for the first adapter: invoking `limactl info` with `LIMA_HOME=/tmp/chamber-lima-home-check` reports that temp path as `limaHome`. `pkg/machine/lima` should set `LIMA_HOME` to `Config.Root/lima` by default so Chamber-owned machine state does not collide with the user's ambient `~/.lima` instances.

## `cmd/ci` Integration Plan

Add a host-side mode to `cmd/ci` without disturbing the existing Linux execution path.

Suggested flags:

- `-machine=auto|none|<name>`: `auto` uses a VM on non-Linux hosts and runs directly on Linux.
- `-machine-provider=lima`: only `lima` initially.
- `-machine-root`: root for Chamber machine adapter state; default outside the workspace, next to the existing CI cache root.
- `-machine-keep`: leave the VM running after CI.

Execution flow:

1. Parse flags.
2. If `-machine=none` or `runtime.GOOS == "linux"`, run the current CI flow unchanged.
3. Otherwise, construct the CI machine spec with the workspace mounted writable.
4. Construct the VM through `lima.New`, which creates/reuses and starts the instance according to config.
5. Reinvoke the same CI command inside the guest with `-machine=none` to avoid recursion.
6. Preserve the caller's requested CI flags that matter inside the guest: image, timeout, keep, workdir, and root.
7. Stream stdout/stderr from the guest process through the host process.
8. Return the guest exit code as the host exit code.

This makes the top-level user flow:

```sh
go run ./cmd/ci
```

on macOS, while preserving:

```sh
go run ./cmd/ci -machine=none
```

inside Linux or when manually debugging a VM.

## Storage And Path Rules

Keep machine state outside the workspace by default. The current CI runner already rejects a CI root contained by the workspace; apply the same invariant to `-machine-root`.

Suggested default:

```text
os.UserCacheDir()/chamber/machines
```

If Lima home isolation works, place Lima state under:

```text
<machine-root>/lima
```

Rendered machine specs can live under:

```text
<machine-root>/specs/<name>.yaml
```

Do not hide workspace mounting inside the adapter. The caller should pass mounts explicitly through `machine.Spec`, so path ownership remains visible at composition boundaries.

## Error Model

Add machine-specific typed errors only where callers need stable behavior:

- invalid name;
- missing provider binary;
- machine not found;
- machine already exists with incompatible spec;
- machine broken;
- command exited non-zero.

Use `pkg/shared/errors.Code` only when the error is part of Chamber's durable public taxonomy. For adapter-local details, wrap the underlying error with enough context and keep stdout/stderr available where useful.

## Tests

Start with unit tests that do not require a VM:

- generic name validation;
- root resolution rejects workspace-contained roots;
- Lima YAML rendering from `machine.Spec`;
- Lima status JSON parsing;
- `lima.New` command sequence for missing, stopped, running, and broken machines using a fake command runner;
- command execution preserves args, workdir, env, stdout, stderr, and exit code;
- `cmd/ci` host wrapper reinvokes itself with `-machine=none`.

Then add opt-in integration tests:

- guarded by an environment variable such as `CHAMBER_LIMA_INTEGRATION=1`;
- creates a disposable machine name;
- runs `uname -s` and verifies Linux;
- verifies the mounted workspace is writable;
- deletes the machine afterward unless a keep flag is set.

## Implementation Phases

### Phase 1: Contract And Rendering

- Add `pkg/machine` public types and validation.
- Add `pkg/machine/lima` adapter scaffolding with a fakeable command runner.
- Implement Lima YAML rendering.
- Add fast unit tests.

### Phase 2: Constructor Lifecycle

- Implement `lima.New`, `Machine.Stop`, and `Machine.Delete`.
- Use `limactl list --format json` for status.
- Keep `LIMA_HOME` rooted under `Config.Root/lima` unless a caller explicitly overrides it.
- Add command-sequence tests for the adapter.

### Phase 3: Remote Command Execution

- Implement `Machine.Run`.
- Preserve exit codes separately from transport/execution errors.
- Stream output instead of buffering by default, because CI output should feel live.
- Add fake-runner tests for env, workdir, and exit-code behavior.

### Phase 4: Dog-Food CI Wrapper

- Add machine flags to `cmd/ci`.
- Keep the current Linux CI function intact and wrap it with host-machine dispatch.
- Move current root resolution helpers only if they become shared; otherwise avoid premature utility packages.
- Verify native compile/tests locally.
- Verify real execution with a Lima smoke test outside the sandbox.

### Phase 5: CLI Polish And Docs

- Update `lima-config.yaml` or replace it with generated config documentation.
- Add a short README section showing host-side CI usage.
- Add troubleshooting notes for missing `limactl`, broken machines, and rootless sysctl/AppArmor provisioning failures.

## Open Design Questions

1. Should `go run ./cmd/ci` automatically provision a VM on macOS, or should the first version require `-machine=auto` until the behavior feels stable?
2. Should Chamber own the full Lima home through `LIMA_HOME`, or only own rendered specs and a machine-name namespace?
3. Should the default machine name be fixed, such as `chamber`, or derived from the workspace path, such as `chamber-<hash>`?
4. Should `cmd/ci` stop the VM after a successful run by default, or keep it warm for faster repeated dog-food cycles?

My conservative answers for the first pass:

- default to `auto` only for `cmd/ci`, because CI already requires Linux and the user's intent is to remove manual `limactl`;
- use a workspace-derived machine name to avoid collisions across Chamber checkouts;
- keep the VM running by default for developer ergonomics, with a clear delete/stop path;
- isolate Lima state with `LIMA_HOME` if the spike proves it reliable.
