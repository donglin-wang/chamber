# Daemon Runtime Supervisor Plan

> Current implementation note: runtime supervision is daemon-owned. The public
> runtime package exposes ordinary primitives such as `Run`, `Open`, and
> `RunAndWait`; the daemon supervisor state file stays private to `daemon`
> because it recovers daemon operations, not general SDK calls.

## Goal

Move Chamber's durable container supervision into `chamberd` without requiring
SDK callers, local tests, or `cmd/github-ci` to build, install, download, or
configure a separate shim binary.

The daemon should supervise containers by spawning a second copy of the daemon
binary in an explicit supervisor mode:

```text
chamberd server process
  -> write daemon-owned supervisor file with phase=prepared
  -> exec same daemon binary with runtime-supervisor --supervisor <path>
      -> supervisor mode reads one supervisor file
      -> supervisor starts and waits for one runtime container
      -> supervisor advances phase=started and phase=completed
```

The public runtime SDK remains the low-level direct execution primitive. The
daemon adds the durable process boundary, operation records, restart
reconciliation, and cleanup policy above it.

## Design Principles

- Keep `pkg/runtime` useful from ordinary Go scripts and tests without daemon
  setup.
- Keep the daemon responsible for daemon-grade reliability: operation records,
  process supervision, recovery, cancellation, and cleanup policy.
- Keep concrete runtime details private behind `internal` packages.
- Do not add a separate supervisor binary for the first draft.
- Do not hide supervisor dispatch in package `init` functions.
- Do not make the daemon hand-assemble `runc` command lines or runtime state
  paths.
- Keep the supervisor process single-purpose: supervise one runtime launch and
  exit.

## Package Shape

Keep the runtime package boring and reusable:

```text
pkg/runtime
  Runtime.Run
  Runtime.Open
  Container.Wait
  RunAndWait(ctx, rt, request, started)
  ContainerResult
```

Keep the daemon supervisor protocol private to `daemon`:

```text
daemon/supervisor.go
  supervisorFile
  runtime-supervisor --supervisor <path>
  reconciliation from daemon metadata and supervisor evidence
```

This boundary keeps daemon operation IDs, daemon recovery policy, file-format
versioning, process reexec, and result application out of the public runtime
SDK. The daemon imports the public runtime factory and root runtime contracts;
it must not import `pkg/runtime/internal/runc`.

## Daemon Mode Shape

Add an explicit daemon subcommand or equivalent mode:

```sh
chamberd runtime-supervisor --supervisor /path/to/supervisor.json
```

In the current repository, this likely means:

```sh
go run ./daemon runtime-supervisor --supervisor /path/to/supervisor.json
```

The normal server mode remains separate:

```sh
go run ./daemon serve ...
```

The important property is that `runtime-supervisor` behaves like a separate
command even though it is compiled into the daemon binary. It should enter only
the supervisor code path and must not initialize or serve the daemon API.

The parent daemon starts the supervisor with the same binary path:

```text
os.Executable()
runtime-supervisor
--supervisor <supervisor path>
```

The parent must not spawn this child through Chamber's normal subprocess helper
if that helper installs parent-death behavior. The supervisor child is
intentionally allowed to survive parent daemon death or restart. Use `os/exec`
directly or a narrowly named subprocess helper mode that opts out of `Pdeathsig`
and process-group behavior that would kill the supervisor with the parent.

This keeps development and release simple:

- `go test` can still run package tests without building a helper binary.
- `go run ./daemon ...` can still exercise daemon behavior.
- CI does not need to download or stage a second supervisor artifact.
- A deployed daemon binary already contains the supervisor mode.

## Supervisor State

The daemon writes one JSON supervisor file before starting the supervisor
process. The file is daemon-owned and durable enough for diagnostics and
recovery. It must not become a public runtime SDK concept.

Supervisor file:

```go
type supervisorFile struct {
    Version int `json:"version"`
    Phase   supervisorPhase `json:"phase"`

    OperationID string `json:"operation_id"`
    ContainerID string `json:"container_id"`

    Runtime runtime.Config `json:"runtime"`
    Bundle  bundle.ProvisionedBundle `json:"bundle"`

    StdoutPath string `json:"stdout_path"`
    StderrPath string `json:"stderr_path"`

    RuntimeName string `json:"runtime_name,omitempty"`
    StartedAt   *time.Time `json:"started_at,omitempty"`
    Result      *runtime.ContainerResult `json:"result,omitempty"`

    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

The supervisor phases are:

- `prepared`: daemon has written the launch input and is about to reexec the
  supervisor child;
- `started`: the runtime confirmed the container started;
- `completed`: the supervisor has a final `runtime.ContainerResult`.

`runtime.Config.Name` selects the concrete runtime implementation. The public
runtime factory validates the config and returns the concrete implementation.

`StdoutPath` and `StderrPath` are daemon evidence logs. They are not the
runtime package's default logs under `RuntimeRoot`. The supervisor helper should
pass these files as additional stdout/stderr writers to `runtime.Run` so daemon
evidence is captured while the direct runtime keeps ownership of its default
runtime logs.

Initial result statuses:

- `exited`: the container process started and exited with an exit code;
- `start_failed`: the supervisor or runtime failed before a container process
  was confirmed running;
- `canceled`: daemon cancellation stopped or deleted the workload;
- `unknown`: recovery could not determine a trustworthy container outcome.

The daemon maps this result back into daemon metadata and operation status. If
the daemon child-process spec includes an operation ID, the result should carry
it as diagnostic metadata too.

## Completion Signals

Supervisor process exit and supervisor-file phase are separate signals. They
should not be treated as one atomic event.

- The supervisor process status is a liveness signal for the monitor.
- `phase=started` is the durable signal that the runtime container actually
  started.
- `phase=completed` with a valid result is the authoritative durable container
  outcome.

The happy path is:

```text
phase=started
supervisor process exits
phase=completed with valid result
daemon records the container outcome
```

Recovery behavior should distinguish the meaningful combinations:

```text
process running, phase=prepared:
  supervisor is alive but container start is not confirmed

process running, phase=started:
  container is running or exiting

process exited, phase=completed:
  container completed and the durable outcome is available

process running, phase=completed:
  durable outcome is available; daemon may wait for or reap the supervisor

process exited, phase=prepared or phase=started:
  supervisor failed before recording outcome; daemon must recover explicitly

process unknown after daemon restart, phase=completed:
  daemon recovers the completed outcome from the supervisor file

process unknown after daemon restart, phase=started:
  daemon has an incomplete running-or-exited workload; use runtime state,
  logs, and policy to recover

process unknown after daemon restart, phase=prepared:
  daemon has an incomplete start; mark start recovery explicitly and clean up
  according to policy
```

The daemon should never mark a container successful merely because the
supervisor process exited. It should require `phase=completed` with a valid
result for a completed container outcome. It should also avoid transitioning a
daemon container from `starting` to `running` until `phase=started` exists and
is valid.

## Daemon Recovery State

The daemon should persist recovery keys and high-level status, not duplicate
private runtime implementation details.

Persist:

- operation ID;
- container ID;
- bundle path;
- supervisor path;
- supervisor started path;
- supervisor result path;
- supervisor PID when known;
- runtime name;
- runtime root;
- stdout and stderr paths;
- high-level daemon status;
- timestamps;
- last error.

Do not persist runc-specific argv, helper internals, or duplicated runtime
state as daemon contract fields. Those can appear in structured logs or
diagnostic files, but the daemon should not depend on them as stable public
metadata.

## Execution Flow

Daemon server process:

1. Accept a container run request.
2. Create daemon operation and container records.
3. Pull or resolve the image.
4. Provision the bundle.
5. Create daemon-owned stdout/stderr paths and optional stdin file.
6. Write `supervisor.json` with `phase=prepared`.
7. Spawn `os.Executable() runtime-supervisor --supervisor <path>` with parent-death
   behavior disabled for this child.
8. Record the supervisor PID in daemon metadata.
9. Return operation/container status to the client.

Daemon supervisor process:

1. Parse `runtime-supervisor --supervisor`.
2. Read and validate the daemon supervisor file.
3. Construct the selected runtime through normal runtime config/factory paths.
4. Open configured stdio files.
5. Call `pkg/runtime.RunAndWait`.
6. The daemon supervisor mode atomically updates the supervisor file to
   `phase=started` after launch succeeds.
7. `RunAndWait` waits for the container result.
8. The daemon supervisor mode atomically updates the supervisor file to
   `phase=completed` with the result.
9. The daemon supervisor mode exits with a status that reflects supervisor
   success or failure.

Daemon reconciliation:

1. Periodically scan daemon records for creating/starting/running containers.
2. Inspect supervisor process records where possible.
3. Read supervisor files for completed supervisors.
4. Use supervisor phase to distinguish unconfirmed starts from running
   workloads.
5. Use public runtime handles such as `Runtime.Open` for state, signal, and
   delete without importing concrete runtime adapters.
6. Mark unknown or incomplete records with explicit recovery states instead of
   silently deleting them.

## Boundary With The SDK

The direct SDK runtime path remains useful:

```go
rt, err := runtimeFactory.NewRuntime(...)
container, err := rt.Run(ctx, request)
result, err := container.Wait(ctx)
```

This path is for low-level point-and-shoot execution by the caller. It should
not require a daemon, supervisor command, `init` hook, or prebuilt helper
binary.

The daemon-supervised path is stronger:

```text
daemon operation
  -> daemon supervisor subprocess
      -> runtime.RunAndWait
```

This path is where Chamber earns daemon-grade lifecycle behavior:

- client disconnect tolerance;
- daemon-side operation status;
- supervisor PID tracking;
- durable started files;
- durable result files;
- daemon restart reconciliation;
- cancellation and forced cleanup policy.

## Cancellation

Cancellation should target the workload before the monitor.

For forced cancellation:

1. Ask the runtime/container control path to stop or force-delete the actual
   container through `Runtime.Open` and `ContainerHandle` methods.
2. Wait for a short bounded timeout for the supervisor to write the result.
3. If the supervisor has not exited, terminate the supervisor process group.
4. Record the daemon cancellation result explicitly.

Killing the supervisor first risks losing the cleanest path for recording the
container result. The supervisor is the monitor; the container is the workload.

## Testing Plan

Fast package tests:

```sh
GOCACHE=/tmp/chamber-go-cache go test ./pkg/... ./daemon/... ./cmd/...
```

Runtime helper tests:

- validate `RunAndWait` result mapping;
- fake runtime execution without a real `runc` binary;

Daemon supervisor mode tests:

- validate supervisor file decoding and phase transitions;
- reject mismatched operation/container IDs;
- verify supervisor-file atomic write behavior;
- invoke supervisor mode with a fake or test supervisor file;
- verify supervisor mode does not initialize or serve the daemon API;
- verify bad supervisor files produce clear failure records;
- verify the parent daemon builds the expected `os.Executable()` command;
- verify the parent daemon opts the supervisor child out of Chamber's normal
  parent-death subprocess behavior.

Linux integration tests:

- run a short successful container through daemon supervisor mode;
- run a failing container and persist the exit code/error;
- restart the daemon while the supervisor process is running;
- kill the supervisor process and verify daemon recovery behavior;
- verify each completion-signal state that can be produced safely in tests;
- force-delete a running container through daemon cancellation.

Native macOS remains a development host only for compile and unit coverage.
Real runtime execution belongs in Linux or Lima.

## Release And Developer Experience

First draft:

- no separate `chamber-runtime-supervisor` binary;
- no `ShimPath` config;
- no `just` or `make` requirement;
- no `go run` subprocess for runtime helpers;
- one daemon binary contains server mode and supervisor mode.

Later, if packaging pressure changes, the daemon-owned supervisor file and
`pkg/runtime.RunAndWait` helper can support a dedicated supervisor binary
without changing runtime-specific implementation details.

## Non-Goals

- No package-level `init` hook for supervisor dispatch.
- No daemon import of concrete runtime adapter packages.
- No public runc-specific supervisor package.
- No supervisor API server, queue, scheduler, or global cleanup authority.
- No cluster or desired-state policy in the supervisor process.
- No rootful daemon boundary expansion as part of this change.

## Deferred Questions

- Whether the public SDK should eventually grow a narrow runtime reconnect or
  control API. The first daemon-supervisor draft should not add one; it should
  recover from daemon metadata, supervisor files,
  process state, and runtime control operations already exposed by `Runtime.Open`
  and `ContainerHandle`.
