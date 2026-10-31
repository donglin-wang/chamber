# GitHub CI end-to-end, concurrency, and robustness coverage

This document describes the implementation inspected on 2026-10-18. It is an
inventory of existing coverage, not a record of a successful test run.

The main robustness matrix is the **14-scenario `daemon-lifecycle` pipeline**.
It executes scenarios sequentially against a real, temporary `chamberd`.
Explicit concurrent-writer tests live in `daemon/metadata`, and CI runs them
through its container-hosted `go test ./...` workloads. There is no configurable
concurrency-level sweep, randomized stress runner, or built-in iteration count
in `cmd/github-ci` today.

## How the CI layers fit together

[`pipeline.Names` and `pipeline.Run`](../cmd/github-ci/pipeline/direct.go)
define three pipelines, in this order:

| Pipeline | Execution path | Main coverage |
| --- | --- | --- |
| `direct-sdk` | CI process composes the image, bundle, and runtime SDKs directly. | Pull a Go image, provision a bundle, bind-mount the checkout at `/workspace`, run `go test ./...`, read logs, and delete execution resources. |
| `daemon-supervised` | CI process calls the configured external `chamberd` over HTTP. | Pull and run through the daemon, poll container state, retrieve stdout/stderr, and persist proof. |
| `daemon-lifecycle` | CI process builds and starts its own temporary `chamberd`. | Exercise lifecycle transitions, interrupted admission, restarts, supervisor loss, client disconnect, stop, decommission, delete, and cleanup. |

The dispatcher runs these sequentially. A pipeline failure is accumulated and
later pipelines are still attempted, unless the parent context is canceled.
[`runCIForPush`](../cmd/github-ci/server.go) supplies a shared overall run
deadline; its default is 30 minutes, covering checkout and the pipeline suite.

All three pipelines run the repository's Go test suite in a container: the
lifecycle pipeline does so in its `chamber-test-suite-daemon-restart` case.
The default image is `docker.io/library/golang:1.26.4-bookworm`.
[`GoTestShellCommand`](../cmd/github-ci/pipeline/direct.go) puts Go build cache,
module cache, and temporary work under `/go/chamber-ci` inside the container.
The daemon-based workloads mount the checkout read-only.

The nested test command is plain `go test ./...`: it does not enable `-race`,
set `-count`, or set `CHAMBER_INTEGRATION=1`. The pipeline requests do not
forward the host integration flag into the container. Consequently, the
ordinary nested workload skips tests gated by that flag, including the two
outer dogfood integration entry points.

## End-to-end entry points and test doubles

| Test | What actually runs | Gate and timeout |
| --- | --- | --- |
| [`TestRunDogfoodIntegration`](../cmd/github-ci/pipeline/direct_test.go) | Real direct-SDK pull/provision/run/test/log/cleanup path. Asserts no runner error and exit code 0. | `CHAMBER_INTEGRATION=1`; runner timeout 30 minutes. |
| [`TestRunDaemonLifecycleIntegration`](../cmd/github-ci/pipeline/daemon_lifecycle_test.go) | Real temporary daemon and the full lifecycle matrix below. Asserts no runner error and exit code 0. | `CHAMBER_INTEGRATION=1` and Linux; runner timeout 45 minutes. |
| [`TestRunDaemonSupervisedWritesProofAndLogs`](../cmd/github-ci/pipeline/daemon_test.go) | An `httptest` daemon returning canned HTTP responses. Checks default image, workspace mount source/target, copied stdout/stderr, and `proof.json` containing `passed: true`. | Ordinary test; configured runner timeout 1 minute. |
| [`TestWebhookRunsCheckoutAndCIAndServesLogs`](../cmd/github-ci/server_test.go) | In-process HTTP handlers with injected checkout, runner, and GitHub status client. Checks requested SHA/remote, admission UUID, pending/success statuses, run records, aggregate and individual logs, and proof-file links. | Ordinary test; no real checkout, container, or GitHub request. |

The production `daemon-supervised` pipeline uses a real external daemon, but
there is no separate gated Go integration test for that real-daemon path in
this directory. The direct integration test calls `RunDirectSDK`, not the
three-pipeline dispatcher.

## The daemon lifecycle matrix

Source: [`RunDaemonLifecycle` and its case helpers](../cmd/github-ci/pipeline/daemon_lifecycle.go).

The runner creates a unique private root under `<CI root>/dl/r-*`, builds
`./daemon`, chooses a loopback HTTP address, and starts the resulting binary.
Image, bundle, runtime, runtime-binary, metadata, and supervisor storage belong
to that temporary authority. It pulls the selected image once before the
scenarios.

Before starting, the runner checks Linux, an unprivileged user, user-namespace
policy, AppArmor policy, and an active user-namespace probe. It also requires
the daemon's reported startup probe to pass the `image`, `bundle`, `runtime`,
`metadata`, `socket`, and `cleanup` scopes.

The following rows are in execution order. State/error expectations describe
the scenario assertion before the final deletion pass.

| Case name | Trigger or fault | Required outcome |
| --- | --- | --- |
| `daemon-restart-after-create-admission` | Send a run request and disconnect; pause the daemon after create admission, stop it, then restart without the pause. | Container becomes `failed` with `runtime_start_failed`. |
| `daemon-restart-after-start-admission` | Create a container, then send a separate start request and disconnect. Pause after durable start admission and restart. | Recovery completes the admitted start; `exited`, code 0, stdout contains `start-admission-recovered`. Create/start operation IDs are recorded. |
| `short-success` | Shell writes one stdout and one stderr message. | `exited`, code 0, empty error code; both log streams are retrieved. |
| `nonzero-failure` | Shell writes stderr and exits 7. | `exited`, code 7, `container_exit_nonzero`; logs remain readable. |
| `create-start-separate` | Create, wait for `created`, then explicitly start. | `exited`, code 0, stdout contains `separate-done`. |
| `daemon-restart-starting` | Start a separately created container, assert the start response says `starting`, then restart the daemon. | `exited`, code 0, empty error code. |
| `chamber-test-suite-daemon-restart` | Run the repository's `go test ./...` container, observe `running`, record its supervisor process, and restart the daemon. | Suite finishes `exited`, code 0; logs are readable and at least four live-log read records were collected. |
| `daemon-restart-after-runtime-exit` | Pause the supervisor after runtime exit, stop the daemon, release the supervisor, wait for its completed evidence, then restart. | The daemon consumes completion evidence: `exited`, code 0, stdout contains `runtime-exit-window`. |
| `client-disconnect-run` | Write a complete raw HTTP run request and close the connection without reading the response. Discover the admitted container through listing. | Work survives client disconnect: `exited`, code 0, stdout contains `disconnected-done`. |
| `daemon-restart-running-supervisor` | Observe a sleeping workload as `running`, verify its supervisor process, then restart the daemon. | Work completes `exited`, code 0, stdout contains `restart-done`. |
| `killed-supervisor-recovery` | Observe `running`, stop the daemon, send SIGKILL to the recorded supervisor PID, then restart the daemon. | Container becomes `failed` with `runtime_wait_failed`; logs are retrieved. |
| `decommission-retains-evidence` | Finish a workload, decommission it, retrieve its logs again, then attempt start. | `decommissioned`; bundle/runtime execution paths are gone; runtime logs and supervisor evidence remain; stdout contains `decommissioned`; start returns HTTP 409. |
| `force-delete-running-container` | Observe a sleeping workload as `running`, then DELETE it. | Subsequent container lookup fails and all tracked execution/log/supervisor artifact paths are absent. |
| `stop-running-container` | Run a shell with a TERM trap, observe `running`, then call stop. | Graceful `exited`, code 0, stdout contains `stopped`. |

The matrix stops at the first failed assertion. Its `proof.json` can therefore
contain only the cases reached before failure.

### Repetition and overlapping activity

This matrix exercises lifecycle timing windows deliberately rather than
launching a batch of competing scenarios:

- Each container wait immediately reads the individual container, then polls
  on a 500 ms ticker, with a five-minute limit per wait and the enclosing
  context deadline.
- When the observed state is `running`, each pass reads stdout and stderr.
  It also attempts a full container list and saves successful list snapshots.
- The repository test-suite case requires at least four live-log records:
  two stdout/stderr pairs, rather than four complete polling rounds.
- The `daemon-supervised` pipeline independently polls the container list on
  a one-second ticker until its container is `exited` or `failed`.
- Admission and runtime-exit pause files make selected restart windows
  deterministic. They avoid relying only on a sleep to hit those windows.

These reads overlap an executing workload and daemon lifecycle work, but the
harness issues its state/log/list requests sequentially. Successful list
snapshots are evidence; list errors inside the lifecycle wait loop are
ignored, so this is not an assertion that every list request succeeds.

### Cleanup assertions

After the scenarios, the runner deletes the remaining 13 containers; the
force-delete case was already deleted. For each deletion it checks that a
container lookup fails and that the tracked bundle, runtime state, stdout,
stderr, and supervisor paths are absent.

It then reads `/v1/operations` and requires zero pending cleanup records and
no operation still in `running`. Finally it stops the daemon, removes the
temporary authority root when `Keep` is false, and checks that the source
snapshot still matches the snapshot used to build the daemon.

These are success-path completion requirements. An earlier failure returns
before the full deletion/root-removal sequence. The direct-SDK pipeline also
attempts cleanup, but some bundle/root/log cleanup errors are logged or
ignored rather than being asserted as strictly as in this matrix.

## Explicit concurrency coverage reached by CI

### Metadata backend contract matrix

[`TestStoreContract`](../daemon/metadata/store_test.go) runs the same contract
against `memory` and embedded `etcd`. Each backend receives all four races:

| Concurrent action | Workers per backend | Assertion |
| --- | --- | --- |
| Create the same operation ID. | 20 | Exactly one succeeds; the other 19 return `metadata.ErrAlreadyExists`. |
| Transition one operation from `running` to `succeeded`. | 20 | Exactly one succeeds; the others return `ErrStateConflict`. Stored state is `succeeded` with a finish timestamp. |
| Compete to finish one operation as `succeeded` or `failed`. | 2 | Exactly one succeeds; the loser gets `ErrStateConflict`. Stored state is one of those terminal states. |
| Transition one container from `running` to `exited`, supplying different exit codes. | 20 | Exactly one succeeds; the others return `ErrStateConflict`. Stored state is `exited` with an exit code. |

`runConcurrently` creates goroutines waiting on the same channel, closes that
channel to release them together, and waits for all results. This gives four
races per backend, eight across the backend matrix, with 62 worker calls per
backend. Each race runs once per contract invocation; worker counts are fixed.

The same contract also checks atomic container/operation admission, atomic
start admission, atomic container/operation transitions, and cleanup lease
behavior. Those checks complement the races but are not additional concurrent
worker batches. `TestContainerValidTransition` and `TestOperationValidTransition`
iterate allowed and forbidden state-transition tables.

### Launch versus reconciliation

[`TestRunContainerHoldsContainerLockFromPreparationThroughLaunch`](../daemon/server_test.go)
starts a run in a goroutine and pauses it after preparation. A second goroutine
tries supervisor reconciliation. The test requires reconciliation to remain
blocked for a 100 ms observation window, then releases preparation and checks
both calls complete with exactly one supervisor launch.

This protects against reconciliation racing the original launch and creating
a duplicate supervisor. It uses fake provisioner/runtime behavior and an
in-memory store, rather than real competing containers.

[`TestCanonicalImageOperationLockKeySerializesEquivalentReferences`](../daemon/locks_test.go)
checks that short and canonical image references produce the same lock key.
It checks lock identity; it does not race two image pulls.

### Recovery and lease matrices

[`TestStartupReconciliationFailsNonterminalContainersWithoutSupervisorEvidence`](../daemon/supervisor_test.go)
is a six-case matrix: `created`, `starting`, and `running`, each paired with
either an empty supervisor path or a missing supervisor file. Every case must
fail the container with `runtime_start_failed` and fail its operation.

Other tests in that file check completed-evidence recovery, prepared-supervisor
ownership, adoption of admitted start operations, mismatched result rejection,
and reused PID identity rejection.

[`TestPeriodicCleanupReconciliationRespectsActiveLease`](../daemon/cleanup_test.go)
first verifies periodic reconciliation preserves an actively leased container,
then verifies startup reconciliation can reclaim and delete it. Neighboring
tests cover reconstruction of cleanup operations and retention of recovery or
decommission evidence. These are focused recovery tests, not stress loops.

## Webhook concurrency and receiver robustness

[`server_test.go`](../cmd/github-ci/server_test.go) covers the receiver's own
concurrency boundary separately from daemon concurrency:

- `TestWebhookReturnsTooManyRequestsWhenSlotBusy` blocks an injected runner
  while the only slot is occupied. The first push gets HTTP 202, the second
  gets HTTP 429, and the test checks the slot is released after completion.
- `TestCancelActiveRunsMarksRunErrored` cancels an admitted runner and checks
  the completed run record and pending-to-error GitHub status transition.
- Startup recovery tests mark unfinished or unreadable run records errored.
  A record without a recoverable SHA does not produce a GitHub status update.
- Retention tests preserve running and unreadable records while pruning an
  old completed record. Request tests check event, repository, signature,
  and deleted-ref handling.
- [`runs_test.go`](../cmd/github-ci/runs_test.go) iterates invalid log paths,
  rejecting traversal and non-text proof files. The receiver composition test
  also checks that binary evidence is omitted from served logs.

`-max-parallel` controls admitted webhook runs in one CI process. The default
is 1; the checked-in [`systemd service`](../cmd/github-ci/github-ci.service)
sets it to 5. Each admitted run still executes its own three pipelines
sequentially. The existing admission test uses one slot; it does not sweep
limits or verify five real jobs running together.

## Evidence to inspect

For webhook runs, [`runCIForPush`](../cmd/github-ci/server.go) supplies:

```text
<CI root>/runs/<run-id>/logs/
  ci.stdout
  ci.stderr
  proof/
    daemon-supervised/
      input.json, healthz.json, pull.json, run.json, polls.json
      stdout.log, stderr.log, proof.json
    daemon-lifecycle/
      chamberd, build.log, daemon-config.json, daemon-probe.json
      daemon-<startup-label>.stdout.log
      daemon-<startup-label>.stderr.log
      pull.json, operations-final.json, proof.json
      <case-name>/
        request/response evidence, polls.json, container.json
        list-polls.json, log-polls.json, stdout.log, stderr.log
```

Files depend on the case and how far execution progressed. `direct-sdk`
contributes to the aggregate CI logs but does not write a structured proof
directory today.

The lifecycle proof includes host/git identity, build-source and binary SHA-256
digests, configured roots, host probes, case records, daemon/supervisor events,
and cleanup outcomes. Case records include operation/container IDs, observed
states, errors, polls, artifact paths, and supervisor process details where
collected. `final_state` reflects the latest recorded observation, so a
decommission case can end with `decommissioned` after its earlier exit check.
The source snapshot covers HEAD, tracked changes, and non-ignored untracked
content; the runner checks it after build and again before passing.

Proof and text logs are available through the run/log HTTP routes. Built
daemon binaries are retained locally as evidence but excluded from text-log
serving.

## Running the coverage

Run from the repository root on Linux, as an unprivileged user, with the Go
toolchain required by `go.mod`. Real container paths require rootless
user-namespace support, subordinate ID mappings/helpers, writable private
storage outside the checkout, and access to required images/runtime downloads.
The lifecycle harness additionally invokes `git`, `go`, and `ps`. On macOS,
run these commands inside the Linux/Lima guest.

Run ordinary CI tests and the daemon tests they exercise:

```sh
CHAMBER_INTEGRATION=0 GOCACHE=/tmp/chamber-go-cache \
  go test -count=1 ./cmd/github-ci/... ./daemon/...
```

Run the real direct-SDK dogfood test:

```sh
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache \
  go test -count=1 -timeout=40m ./cmd/github-ci/pipeline \
  -run '^TestRunDogfoodIntegration$' -v
```

Run the real lifecycle matrix, keeping proof outside the checkout:

```sh
CHAMBER_INTEGRATION=1 \
  CHAMBER_DAEMON_LIFECYCLE_PROOF_DIR=/var/tmp/chamber-lifecycle-proof \
  GOCACHE=/tmp/chamber-go-cache \
  go test -count=1 -timeout=55m ./cmd/github-ci/pipeline \
  -run '^TestRunDaemonLifecycleIntegration$' -v
```

The lifecycle test optionally accepts `CHAMBER_DAEMON_LIFECYCLE_IMAGE`.
Without a proof override it uses the user cache directory under
`chamber/daemon-lifecycle-proof`; both real integration tests use
`chamber/ci-integration` under the user cache as their CI root.
The longer explicit Go test timeouts above allow the runner deadlines to work;
the standalone lifecycle test's 45-minute timeout differs from the webhook
suite's default overall 30-minute timeout.

For an additional repeated race-detector run of the existing focused tests:

```sh
CHAMBER_INTEGRATION=0 GOCACHE=/tmp/chamber-go-cache \
  go test -race -count=25 -timeout=10m ./daemon/metadata \
  -run '^TestStoreContract$'

CHAMBER_INTEGRATION=0 GOCACHE=/tmp/chamber-go-cache \
  go test -race -count=25 -timeout=10m ./daemon \
  -run '^TestRunContainerHoldsContainerLockFromPreparationThroughLaunch$'
```

These last commands add repetition and race detection manually; they are not
the default GitHub CI workload. Repeated lifecycle runs should use a distinct
proof directory for each invocation because evidence filenames are reused.

## Limits of the current claims

- The lifecycle matrix uses one selected image and one host configuration per
  invocation. It is not a cross-product of runtimes, platforms, privilege
  modes, concurrency levels, or fault timings.
- Daemon restart helpers normally send SIGTERM and escalate to kill only if
  shutdown exceeds ten seconds. Even cases named for admission crashes use
  that stop helper. Supervisor loss explicitly uses SIGKILL; daemon power-loss
  or SIGKILL behavior at every window is not established by this matrix.
- The real matrix checks stop and client disconnection. It does not exercise
  a daemon operation-cancel API; receiver cancellation is covered with a fake
  CI runner.
- A failed post-delete lookup is accepted as evidence of absence without
  specifically asserting HTTP 404. Live list errors are also tolerated as
  described above.
- The matrix does not run simultaneous real pull/run/stop/delete/GC request
  batches, sustained load, or a long-running randomized soak. The focused
  concurrent-writer and launch-lock tests establish narrower invariants.

[`github-ci-plan.md`](github-ci-plan.md) and the
[`ecosystem roadmap`](chamber-ecosystem-roadmap.md) provide design context.
Their proposed checks should be distinguished from the implemented assertions
and gates described here.
