# Chamber Ecosystem Roadmap

Chamber should grow into a programmable, rootless, node-local execution
substrate. CI, local distributed-system clusters, fault-injection testing, and
production deployment are natural uses of the same substrate if Chamber keeps
the layers honest:

1. `pkg/` exposes reusable build, image, bundle, runtime, network, and
   filesystem primitives.
2. `chamberd` composes those primitives into one reliable local authority.
3. Higher-level tools use `chamberd` for CI, cluster tests, and deployment
   policy.

The product should not become a Kubernetes clone, a Docker replacement, or a
distributed scheduler hidden inside the low-level SDK. Distributed placement,
global queues, desired-state policy, and fleet-level coordination belong above
the node-local daemon.

## North Star

A Go user should be able to write:

- a CI runner that checks out a repo, starts an isolated Chamber container,
  collects logs, and reports status;
- a local cluster test that starts N Chamber containers on a private network;
- a fault-injection test that partitions, delays, kills, restarts, and inspects
  cluster members from Go;
- a production controller that tells one or more `chamberd` instances what
  should keep running.

The same lower-level contracts should power all of those flows. If the CI
runner, cluster test harness, and production controller each need their own
container lifecycle model, Chamber has leaked its core abstraction.

## Dogfood Standard

Each roadmap phase has two gates:

1. **Implementation gate:** conventional unit, contract, and integration tests
   pass in Linux.
2. **Dogfood gate:** Chamber uses Chamber as the test infrastructure for the
   new behavior.
3. **Host probe gate:** the relevant host-assumption probes pass before the
   dogfood run starts.

The dogfood gate is what makes a phase "embarrassingly solid." It should leave
behind enough evidence that a later reader can understand what ran:

- exact git SHA;
- Chamber version or binary path;
- host identity and kernel summary;
- Chamber root paths;
- operation IDs;
- container IDs;
- network IDs when applicable;
- stdout and stderr logs;
- host-assumption report;
- final status records;
- cleanup results;
- crash or restart evidence when recovery is part of the claim.

Conventional tests may prove a package is internally coherent. Dogfood tests
prove the package composes with image storage, bundle provisioning, runtime
execution, daemon metadata, logs, cleanup, and rootless host policy.

## Host Probe Contract

`docs/host-assumption-validator-plan.md` is part of this roadmap, not a
separate nice-to-have. Chamber should expose package-scoped host checks before
it claims point-and-shoot behavior for build, provision, run, network, or daemon
flows.

The contract is:

- if a user runs the probe for a package scope and it passes, the machine should
  be able to run that package's normal active operation;
- if the machine cannot run that operation after a passing probe, Chamber should
  treat it as a Chamber bug, a stale probe, or a missing probe dimension;
- if host policy will block the operation, the probe should fail first with a
  precise finding and remediation;
- constructors still validate Chamber config and private directories, but the
  validator owns host capability and active probes;
- static checks are useful for fast diagnostics, but an "embarrassingly solid"
  claim requires active probes.

This specifically includes AppArmor and other LSM policy. A user who passes the
runtime probe should not later discover that `runc init` cannot unshare
namespaces because Ubuntu AppArmor blocked unprivileged user namespaces. That
must be reported as unsupported host policy before `runtime.Run`.

Initial scopes should align with package boundaries:

- `build`: BuildKit, RootlessKit, managed helper binaries, rootless build
  namespaces, snapshotter behavior, registry access when requested, and
  cross-architecture support when requested.
- `provision`: readable OCI layout, rootless unpack behavior, whiteouts,
  links, ownership mapping, private bundle roots, and bundle spec validation.
- `run`: runc availability, managed runc installation, user namespace support,
  AppArmor/LSM compatibility, cgroup mode, namespace creation, runtime roots,
  stdio logs, wait status, signal, and delete.
- `network`: user-scoped network roots, namespace or helper availability,
  attach/detach behavior, connectivity, isolation, and cleanup.
- `daemon`: all package scopes selected by daemon config, plus socket roots,
  metadata storage, operation records, restart reconciliation, and cleanup
  authority.

Every dogfood run should persist the validator report next to its operation
logs. The report is evidence that the host was supposed to support the package
being tested.

## Dogfood Rings

Use increasingly strict rings as Chamber becomes capable of hosting more of
its own validation:

### Ring 0: Linux Compile And Unit Tests

Run Go tests directly on Linux or inside a Linux VM. This catches ordinary
package bugs but does not prove Chamber can host the workload.

Required while bootstrapping:

```sh
GOCACHE=/tmp/chamber-go-cache go test ./pkg/... ./daemon/... ./cmd/...
```

### Ring 1: SDK-Hosted Test Container

Use the SDK path from `cmd/github-ci`: pull a Go image, provision a bundle,
mount the checkout at `/workspace`, run `go test ./...`, collect logs, and
remove the container.

This proves image, bundle, runtime, bind mounts, logs, and cleanup work together
without depending on `chamberd`.

Required probes: `build` when building the test image, `provision`, and `run`.
For pull-only CI, `provision` and `run` are mandatory, while registry access is
validated as part of image pull behavior.

### Ring 2: Daemon-Hosted Test Container

Run the same repo test workload through `chamberd` instead of direct SDK
composition. The daemon owns operation records, state transitions, log paths,
and cleanup.

This proves the local authority can host Chamber's own CI workload.

Required probes: `daemon`, including the selected image, bundle, runtime,
metadata, socket, and cleanup scopes.

### Ring 3: Chamber-Hosted Cluster Test

Use Chamber to create a networked cluster of containers that runs a real
distributed test workload. The workload should include a coordinator, several
nodes, readiness checks, logs, and teardown.

This proves Chamber can be the substrate for local distributed-system tests.

Required probes: `daemon`, `network`, `provision`, and `run`.

### Ring 4: Chamber-Hosted Fault Test

Use Chamber to start a cluster, apply faults through Chamber-controlled runtime
or network handles, assert recovery behavior, collect logs, and destroy the
cluster.

This proves the cluster API is programmable enough for serious fault testing.

Required probes: Ring 3 probes plus active fault-capability probes for every
fault used by the test.

### Ring 5: Chamber-Hosted Long-Running Service

Use a controller above `chamberd` to keep Chamber-owned services running across
process restarts and host reboot. This is the first production-deployment ring.

This proves the production story is more than a one-shot demo.

Required probes: Ring 3 probes plus daemon restart reconciliation and cleanup
probes for the selected production profile.

## Phase 0: Host Assumption Validator

### Goal

Implement the validator described in
`docs/host-assumption-validator-plan.md` and make package probes the front door
for Chamber's point-and-shoot promise.

### Implementation Plan

- Add a validator API separate from image, bundle, runtime, network, and daemon
  constructors.
- Implement static and active probe modes.
- Start with `provision` and `run` because they are the shortest path to
  eliminating surprise `runc init` and AppArmor failures.
- Add `build` probes for BuildKit before treating Dockerfile builds as
  point-and-shoot.
- Add `network` probes before the first public network provider is considered
  usable.
- Add a `daemon` aggregate probe that runs the package probes implied by daemon
  config and checks daemon-owned socket, metadata, operation, and cleanup roots.
- Give every fatal finding a stable code, clear message, and remediation.
- Persist probe reports as JSON so dogfood runs and CI can archive them.
- Make active probes use Chamber-owned roots and leave no host state behind
  except the report.
- Treat any later operation failure on a host with a passing active probe as a
  test gap until proven otherwise.

### Embarrassingly Solid Dogfood Proof

Use Ring 0 plus the smallest active package probes. On a supported Linux host,
the validator should run:

- `provision` probe against a known tiny OCI layout;
- `run` probe against a known tiny bundle that exits successfully;
- `build` probe with a tiny Dockerfile containing `RUN true`;
- negative AppArmor/user-namespace detection on a host or fixture where that
  policy is known to block rootless execution;
- report persistence and cleanup verification.

Then use Ring 1 only after the required probes pass. If Ring 1 fails with a
host-policy error that the probes did not catch, Phase 0 is not complete.

This phase is not solid until the validator prevents the known class of
"everything looked fine until AppArmor/runc/BuildKit failed halfway through"
incidents.

## Phase 1: Runtime Becomes Reconnectable

### Goal

Move `pkg/runtime` from direct `runc run` process ownership toward a
shim-like lifecycle model. A runtime container should be controllable after the
original caller process exits.

### Implementation Plan

- Introduce a small Chamber-owned runtime shim process or equivalent supervisor.
- Persist runtime-owned state under `RuntimeRoot`, including pid, status,
  stdout path, stderr path, started time, exit code, exit time, and error.
- Add a reconnect/open API such as `Runtime.Open(ctx, containerID)` or an
  equivalent runtime-owned container lookup.
- Keep stdio and log ownership inside the runtime package.
- Keep daemon operation records out of `pkg/runtime`.
- Ensure `State`, `Signal`, `Delete`, `ReadLog`, and `Wait` can work after the
  original caller reconnects.
- Preserve the public `os.Signal` vocabulary for container signaling.

### Embarrassingly Solid Dogfood Proof

Use Ring 1 after a passing `run` probe. Run Chamber's own test suite inside a
Chamber-launched container, then intentionally kill the host-side Go process
that launched the container. A second Chamber process must reconnect to the
runtime container, read logs, wait for completion, delete runtime state, and
prove no runtime-owned files are left except retained evidence.

The proof should include:

- one short successful container, such as `echo ok`;
- one long-running container that survives caller exit;
- one Chamber CI container running `go test ./...`;
- reconnect after caller exit;
- reconnect after a new Chamber binary process starts;
- successful signal and forced delete;
- stdout/stderr log reads before and after reconnect;
- cleanup verification under the runtime root.

This phase is not solid until `Wait` no longer depends on an in-memory
`exec.Cmd` handle owned by the original caller, and until any post-probe
runtime host-policy failure is treated as a missing validator check.

## Phase 2: Daemon Owns Durable Local Lifecycle

### Goal

Make `chamberd` the reliable authority for one node. It should survive client
disconnects, daemon restarts, and ordinary container exits without losing
truth.

### Implementation Plan

- Split container lifecycle into create, start, run, stop, remove, list, state,
  logs, and cancel operations.
- Add daemon startup reconciliation for `creating`, `starting`, and `running`
  container records.
- Reconnect daemon records to runtime containers through the new runtime open
  API.
- Add operation-scoped locks for image references, containers, and cleanup.
- Add stop/remove endpoints before adding richer orchestration.
- Add log tailing only after log ownership and read behavior are stable.
- Make bundle and runtime cleanup lease-aware.
- Keep the daemon API over a user-scoped Unix socket by default; keep TCP/HTTP
  as an explicit development/demo option.

### Embarrassingly Solid Dogfood Proof

Use Ring 2 after a passing `daemon` probe. Start `chamberd`, ask it to run
Chamber's own test suite in a container, then restart `chamberd` while the test
container is still running. The restarted daemon must reconcile records,
reconnect to runtime state, return logs, record the final exit code, and clean
up.

The proof matrix should cover:

- daemon restart while a container is `starting`;
- daemon restart while a container is `running`;
- daemon restart after the runtime process has already exited but before the
  daemon recorded completion;
- client disconnect immediately after `POST /containers/run`;
- stop and remove of a running test container;
- cleanup after successful exit;
- cleanup after forced delete;
- repeated list/log/state calls during the run.

This phase is not solid until stale `running` records are treated as a normal
startup reconciliation path, not an incident, and until daemon startup refuses
to advertise readiness when required package probes fail.

## Phase 3: Add `pkg/network`

### Goal

Add a narrow public network SDK package that can create user-scoped networks
and attach Chamber containers to them.

### Implementation Plan

- Add `pkg/network` with root contracts, config, fixed status strings, and
  package-level validation helpers.
- Extend the host validator with a `network` scope before declaring the provider
  ready.
- Add `pkg/network/factory` for constructor dispatch.
- Keep concrete providers under `pkg/network/internal/<provider>`.
- Start with one local rootless provider.
- Model stable concepts:
  - `Network`;
  - `Endpoint`;
  - `Provider`;
  - `CreateNetwork`;
  - `Attach`;
  - `Detach`;
  - `Remove`.
- Make network roots and temp roots user-scoped and explicit.
- Make network names and endpoint IDs private to the owning Chamber root.
- Decide the bundle handoff deliberately: network attachment likely needs to
  produce OCI spec changes before runtime launch.
- Do not add distributed networking, service discovery, or multi-host routing
  in this phase.

### Embarrassingly Solid Dogfood Proof

Use Ring 3 after passing `network`, `provision`, and `run` probes. Start with
the smallest real cluster: two Chamber containers on one Chamber-created
network.

The test should:

- create a network;
- start a server container;
- start a client container;
- prove client-to-server traffic works through the Chamber network;
- prove containers from another user's Chamber root cannot accidentally attach
  or collide by name;
- collect server and client logs;
- remove the containers;
- remove the network;
- verify no network namespace, socket, pid, temp, or metadata state remains
  under the Chamber roots.

This phase is not solid until network cleanup is repeatable after both success
and failed container startup, and until the network probe catches missing host
helpers or namespace policy before the first attach attempt.

## Phase 4: Daemon-Aware Networking

### Goal

Teach `chamberd` to own network records and coordinate network attach/detach
with container lifecycle.

### Implementation Plan

- Add daemon metadata records for networks and endpoints.
- Add daemon network create/list/remove endpoints.
- Add container create/run request fields for network attachments.
- Persist the exact network attachment data used to provision the bundle.
- Reconcile network attachments on daemon startup.
- Add container and network operation locks so cleanup cannot race attach.
- Keep network provider policy out of global daemon config except through
  public `pkg/network.Config`.

### Embarrassingly Solid Dogfood Proof

Use Ring 3 through the daemon API after a passing aggregate `daemon` probe.
Start `chamberd`, create a network, run a two-node test workload, restart
`chamberd`, and prove the daemon can still list the network, list attached
containers, read logs, and cleanly remove everything.

The proof matrix should cover:

- daemon restart after network create but before any container attaches;
- daemon restart after one endpoint attaches;
- container start failure after endpoint allocation;
- forced container removal with attached endpoint;
- network removal blocked while attached containers still exist;
- successful network removal after detach.

This phase is not solid until network state is reconciled with the same rigor
as runtime state, and until daemon readiness includes network probe status when
networking is enabled.

## Phase 5: Cluster Harness For Go Tests

### Goal

Provide a Go-native harness for local distributed-system tests. Users should be
able to describe a cluster in code, run it through Chamber, gather evidence,
and tear it down.

### Implementation Plan

- Add a package or example that composes daemon APIs into a test harness.
- Keep it above `pkg/`; do not put cluster policy into low-level SDK packages.
- Support:
  - image pull or build;
  - network creation;
  - N named containers;
  - stable per-node environment;
  - readiness checks;
  - log collection;
  - artifact collection;
  - teardown.
- Make failure evidence first-class: every cluster run should have a run
  directory with metadata and logs.
- Add examples with a small distributed workload, not only `sleep`.

### Embarrassingly Solid Dogfood Proof

Use Chamber to run a distributed test of Chamber's own support code after all
selected cluster probes pass. The first version can use a tiny Go HTTP or TCP
cluster built from the Chamber checkout: one coordinator and three workers, all
launched by Chamber.

The test should:

- build or pull the test workload image using Chamber;
- start a private network;
- start all nodes through `chamberd`;
- wait for readiness;
- run a real client workload;
- collect each node's logs;
- assert expected responses;
- tear down all containers and the network;
- run the same test repeatedly without name or root collisions.

This phase is not solid until cluster teardown is reliable enough that a failed
test can be rerun immediately on the same host without manual cleanup, and until
cluster harness failures clearly distinguish host-probe failures from workload
failures.

## Phase 6: Fault Injection

### Goal

Make Chamber useful for distributed-system failure testing by exposing
programmable runtime and network faults through Go interfaces.

### Implementation Plan

- Add capability discovery to network and runtime providers.
- Extend the validator so fault capabilities can be actively probed before a
  test asks for them.
- Start with a small fault vocabulary:
  - stop;
  - kill;
  - restart;
  - pause or freeze if supported;
  - network partition;
  - latency;
  - packet loss;
  - port drop.
- Keep fault injection provider-specific behind interfaces.
- Make faults scoped to Chamber-owned containers and networks.
- Record every injected fault as structured evidence.
- Make fault cleanup idempotent.

### Embarrassingly Solid Dogfood Proof

Use Ring 4 after passing the probes for every requested fault. Run a
Chamber-hosted cluster test, inject a network partition and a container kill,
assert the workload recovers or fails in the expected way, then remove every
fault and prove the cluster returns to a healthy state.

The proof should cover:

- partition one node from the rest;
- heal the partition;
- kill one node;
- restart the node;
- collect logs around fault timestamps;
- verify fault state after daemon restart;
- cleanup faults before network/container removal;
- reject attempts to apply faults to non-Chamber-owned host resources.

This phase is not solid until failed fault tests leave enough evidence to
debug the cluster without rerunning immediately, and until unsupported fault
mechanisms fail at probe time instead of halfway through a test.

## Phase 7: CI Becomes Daemon-Backed

### Goal

Keep `cmd/github-ci` as a real dogfood app, but move it from direct SDK
composition to daemon-backed execution once the daemon is reliable.

### Implementation Plan

- Run one local `chamberd` on the CI worker host.
- Have `cmd/github-ci` submit jobs to the daemon instead of calling image,
  bundle, and runtime packages directly.
- Preserve exact checkout behavior and container-local Go runtime/cache state.
- Store CI run metadata separately from daemon operation metadata.
- Link GitHub status details to aggregate run logs.
- Keep GitHub credentials and webhook HMAC secrets separate.
- Keep concurrency gating outside low-level SDK packages.

### Embarrassingly Solid Dogfood Proof

Use Ring 2 as the normal CI path for Chamber itself, with the aggregate
`daemon` probe archived for every run. A GitHub webhook should cause the CI
receiver to ask `chamberd` to run `go test ./...` inside a Chamber container.
The GitHub status link should point to logs gathered from daemon
operation/container records.

The proof should cover:

- signed webhook validation;
- exact commit checkout;
- daemon-backed Chamber test container;
- visible pending/success/failure status;
- whole-run log endpoint;
- daemon restart during a CI run;
- CI receiver restart during a CI run;
- retention cleanup that does not delete active daemon state.

This phase is not solid until Chamber's default CI for Chamber itself runs
through `chamberd`, and until host-unsuitable failures are reported as probe
failures before the CI job starts.

## Phase 8: Single-Node Shepherd

### Goal

Introduce a higher-level controller that keeps a desired number of containers
running on one Chamber node.

Use the spelling `shepherd` unless the project intentionally chooses a
different product name.

### Implementation Plan

- Keep `shepherd` above `chamberd`.
- Define a small imperative API first:
  - start one service;
  - scale to N replicas;
  - stop a service;
  - replace image digest;
  - inspect service state.
- Add health checks and restart policy.
- Pin images by digest for production runs.
- Store desired service records separately from daemon container records.
- Let `chamberd` remain the authority for actual local containers.
- Avoid multi-node placement until the single-node model is boring.

### Embarrassingly Solid Dogfood Proof

Use Ring 5 on one host after passing the selected production profile probe. Run
a small service under `shepherd`, kill one replica, restart `shepherd`, restart
`chamberd`, and prove the desired replica count is restored without orphaning
containers.

The proof should cover:

- scale from zero to N;
- kill one replica and observe replacement;
- restart `shepherd`;
- restart `chamberd`;
- host reboot if available;
- image update from digest A to digest B;
- graceful stop;
- forced cleanup after a bad deployment.

This phase is not solid until the controller can explain every running
container as either desired, exiting, or garbage eligible, and until it refuses
to start a service whose required host/package probes fail.

## Phase 9: Multi-Node Shepherd

### Goal

Extend production deployment beyond one machine without moving distributed
scheduler responsibilities into `chamberd`.

### Implementation Plan

- Treat each `chamberd` as one node-local authority.
- Put node inventory, placement, service desired state, and rollout policy in
  the shepherd layer.
- Start with static node membership.
- Add explicit node drain and cordon commands.
- Add simple placement constraints only after static placement works.
- Do not add global queues, consensus, or auto-scaling in the first multi-node
  pass.

### Embarrassingly Solid Dogfood Proof

Use Chamber to deploy and test Chamber-controlled services across at least two
Linux machines or Linux VMs, each with its own persisted probe report. The test
should intentionally remove one node from service and prove the shepherd can
explain and repair the placement state.

The proof should cover:

- two independent `chamberd` instances with separate roots;
- static membership;
- deploy N replicas across nodes;
- stop one daemon;
- drain one node;
- replace a failed container on an allowed node;
- collect logs from all nodes;
- remove the service and verify each node's cleanup.

This phase is not solid until node loss produces an understandable degraded
state instead of silent drift, and until placement avoids nodes that do not pass
the required package probes for the service.

## Non-Goals Until The Core Is Solid

- Kubernetes-compatible APIs.
- CRI compatibility.
- Docker Compose compatibility.
- Multi-host networking as part of `pkg/network`.
- Automatic secret distribution.
- Global desired-state reconciliation inside `chamberd`.
- Fleet-wide garbage collection in the daemon.
- Rich service discovery before basic network identity is stable.
- Rootful daemon mode without an explicit privilege-boundary design.

## Immediate Next Work

The next practical sequence is:

1. Implement the host validator API and report format.
2. Add active `provision` and `run` probes, including AppArmor/user-namespace
   detection.
3. Require those probes before runtime dogfood runs.
4. Design the runtime reconnect API and shim state layout.
5. Implement the runtime dogfood reconnect test.
6. Add daemon startup reconciliation on top of reconnectable runtime state.
7. Add daemon stop/remove/log/state endpoints.
8. Add the aggregate daemon probe.
9. Design the minimal `pkg/network` contracts and network probe.
10. Build the first two-container Chamber network proof.

The product line should stay narrow while these are in progress. If a proposed
feature cannot be validated by one of the dogfood rings above, it is probably
too high-level for the current stage.
