# Chamber Plan

## Purpose

Chamber is a rootless execution daemon for distributed automation platforms. It should provide a single binary that starts a per-machine, per-user daemon, accepts container execution requests from that user's local clients or a higher-level coordinator agent running as that user, and coordinates image pull, container run, logs, cancellation, state, and cleanup without requiring `sudo`.

Chamber is the execution substrate, not the distributed control plane. A future orchestration layer can schedule work across machines, reconcile desired state, and coordinate placement. Chamber's job is to make each user account on each node a reliable, recoverable, unprivileged container execution target.

The goal is not to build a general container engine, reimplement every low-level container primitive, or build a Kubernetes replacement inside the node daemon. The goal is to assemble proven OCI/container libraries behind a daemon that owns the controller-facing reliability contract: durable operations, node-local state, scheduling, locking, leases, cancellation, logs, recovery, and cleanup semantics.

## Problem Statement

Existing rootless container tools are often optimized around CLI-driven workflows on one machine. Distributed automation needs a stronger node-level contract: a coordinator may issue overlapping pulls, runs, cancellations, log reads, deletes, retries, health checks, and cleanup requests against the same worker at high frequency.

When many independent commands or controller actions mutate the same image store, container store, runtime state, and cleanup state at the same time, timing windows appear around operations such as pull, list, run, cancel, remove, and prune.

Chamber should be built around the assumption that many clients, including a future distributed coordinator, may issue overlapping requests to the same user's daemon. The daemon must make user-local node concurrency explicit instead of treating it as an incidental consequence of many CLI processes sharing the same storage.

Chamber must also assume that multiple users on the same machine may each run their own Chamber daemon at the same time. Those daemons must not share mutable state, sockets, locks, runtime directories, temporary directories, container names, network allocations, or cleanup authority unless an explicit future cross-user feature is designed around that boundary.

## Non-Goals

- Do not depend on containerd.
- Do not require root or `sudo` for normal operation.
- Do not implement distributed scheduling, cluster membership, placement, or desired-state reconciliation in the node daemon.
- Do not initially target Kubernetes CRI compatibility.
- Do not implement a full OCI runtime from scratch when `runc`, `crun`, or another OCI runtime can be used.
- Do not implement every Docker or Podman feature in the first release.
- Do not implement image build or image push in the first release.
- Do not implement Docker API, Podman API, Compose, or Kubernetes compatibility in the first release.
- Do not implement rich container networking in the first release beyond what is required to run common automation jobs safely.
- Do not implement general volume management in the first release; allow only carefully scoped bind mounts if needed.
- Do not treat pruning or cleanup as a best-effort shell command; cleanup must be lease-aware.

## Core Principles

1. **Single authority over mutable state**

   One daemon owns one OS user's node-local container state, image references, active leases, runtime allocations, and cleanup decisions.

2. **Rootless by default**

   The daemon and containers run as an unprivileged user using user namespaces, rootless networking, and rootless-compatible storage. This allows a distributed automation platform to run workers without giving the platform root-equivalent access to every host.

3. **Concurrency is a product feature**

   Parallel requests from local clients and distributed controllers should be expected. The server should use explicit scheduling, leases, operation queues, and state transitions to avoid races.

4. **Reuse proven libraries**

   Pull, auth, image format handling, OCI spec generation, runtime execution, and minimal rootless networking should come from existing mature libraries where practical.

5. **Coordinator-friendly API**

   The runtime should expose a stable node API that is useful to local tools and to a future distributed control plane. The first API should model execution operations needed by automation controllers, not the full surface area of Docker, Podman, or Kubernetes.

6. **Crash recovery matters**

   The daemon should tolerate process crashes and restart into a coherent state without requiring destructive resets.

7. **Storage placement is explicit**

   Chamber should account for every path it writes to, including temporary files, runtime state, logs, layer data, unpacked root filesystems, sockets, and lock files. Users should control storage through Chamber-specific configuration and flags, not broad environment variables that affect unrelated programs.

8. **User isolation is a first-class boundary**

   A Chamber daemon belongs to exactly one OS user on exactly one machine. Two users running Chamber on the same host should behave like two independent installations: separate config, socket, state database, image store, container store, runtime state, network bookkeeping, locks, logs, temporary space, and garbage collection.

## Proposed Architecture

```text
same-user local clients / same-user node-local coordinator agent
  |
  | HTTP/gRPC over Unix socket
  v
chamber per-user node daemon
  |
  +-- API layer
  +-- operation scheduler
  +-- state store
  +-- lease manager
  +-- image manager
  +-- container manager
  +-- minimal network/runtime integration
  +-- garbage collector
  |
  +-- OCI runtime: crun/runc
  +-- image/runtime/storage libraries
```

## Coordinator Boundary

Chamber should make a single user-scoped runtime on a single node dependable. It should not decide which node should run a job, maintain cluster membership, perform global reconciliation, or own distributed queues.

A higher-level automation platform can sit above Chamber and use it as the node execution API. That layer can decide placement, retries, desired state, admission policy, cross-node networking, secrets distribution, and fleet-level observability. If the platform runs workers under multiple OS users on the same machine, it should treat each user's Chamber daemon as a separate execution target.

Chamber should expose enough node-local truth for that layer to work well:

- node capabilities and health;
- active operations and their progress;
- image and container state;
- resource usage and pressure signals;
- event streams;
- durable operation IDs;
- cancellation and cleanup semantics.

## Major Components

### API Server

Expose a local Unix socket API for same-user clients and same-user node-local coordinator agents. Start with a stable internal API before attempting Docker-compatible, Podman-compatible, or cluster-facing compatibility surfaces.

The API socket must be user-scoped. Defaults should use a per-user runtime directory, such as `$XDG_RUNTIME_DIR/chamber/chamber.sock` when available, and must not use a global machine path such as `/var/run/chamber.sock`. Socket permissions should prevent other users from issuing requests to the daemon by default.

Initial operations:

- `ImagePull`
- `ImageList`
- `ImageRemove`
- `ContainerCreate`
- `ContainerStart`
- `ContainerRun`
- `ContainerStop`
- `ContainerRemove`
- `ContainerList`
- `ContainerLogs`
- `OperationCancel`
- `OperationStatus`
- `SystemInfo`
- `SystemEvents`

### State Store

Persist daemon-owned, user-scoped node state in a local database. The store should track:

- images
- tags
- manifests
- layers
- containers
- snapshots/root filesystems
- minimal network/runtime allocations
- allowed bind mounts, if supported
- leases
- operations
- events

Important requirement: every mutating operation should have a durable operation record so the daemon can recover or roll back after restart.

### Lease Manager

Leases are the main defense against destructive concurrency bugs.

Examples:

- A running container holds a lease on its image, layers, root filesystem, allowed mounts, and runtime allocation.
- A pull holds a temporary ingest lease until the image is committed into the local store.
- Garbage collection can only delete objects without active leases.

### Image Manager

Responsible for:

- registry auth
- image pull
- manifest resolution
- tag management
- content verification
- local content store coordination
- unpacking layers into snapshots/root filesystems

Candidate libraries to evaluate:

- `github.com/containers/image`
- `github.com/containers/storage`
- ORAS libraries
- distribution/reference libraries

### Container Manager

Responsible for:

- OCI spec generation
- root filesystem preparation
- process lifecycle
- attach/log handling
- signal delivery
- exit status tracking
- cleanup

Candidate libraries/tools:

- `crun`
- `runc`
- OCI runtime-spec
- OCI runtime-tools

### Minimal Network and Runtime Integration

Responsible for enough rootless runtime integration to execute common automation jobs safely.

Candidate tools/libraries:

- `pasta`
- `slirp4netns`

First-release networking should stay narrow:

- outbound network access, if required by the target jobs
- no general container network management API
- no cross-container networking
- no cross-node networking
- per-container network namespace lifecycle
- deterministic cleanup

### Garbage Collector

Garbage collection must be coordinated by the daemon and lease-aware.

Rules:

- Never remove content, snapshots, runtime allocations, or minimal network state with active leases.
- Prefer mark-and-sweep over ad hoc deletion.
- Separate user-requested deletion from physical cleanup.
- Make cleanup resumable after daemon restart.

## Concurrency Model

Chamber should support high request concurrency within each per-user daemon while serializing only the operations that truly conflict.

Examples:

- Listing images should not fail because an unrelated image is being removed.
- Pulling different images should proceed concurrently.
- Pulling the same image should deduplicate or join the active operation.
- Removing an image should mark the reference deleted, then allow GC to reclaim storage only after leases expire.
- Starting containers from the same image should proceed concurrently once the image is available.
- Prune should never race active pulls, runs, removals, or cancellations.

Use operation-scoped locks instead of one global lock where possible:

- per-image reference lock
- per-content-object lock
- per-container lock
- per-runtime/network allocation lock, if minimal networking is enabled
- global GC coordination lock

## Rootless Requirements

The daemon should run as a normal user and store data under Chamber-controlled, user-configured, user-private paths. Chamber must support multiple daemons running concurrently on the same machine under different OS users without coordination through shared writable paths.

Primary path controls:

- `--config`: Chamber config file path
- `--root`: persistent Chamber storage root
- `--run-root`: runtime Chamber storage root for sockets, pid files, locks, and short-lived state
- `--tmp-root`: temporary Chamber storage root for pulls, extraction, runtime setup, and helper scratch space

If defaults are provided, they must be documented concrete paths derived from the current OS user, standard per-user base directories, or the user's explicit Chamber config. Chamber must not require changing global process environment variables such as `HOME`, `TMPDIR`, `TMP`, or `TEMP` to redirect storage.

Default paths should avoid global shared locations. A reasonable Unix default shape is:

- config: `$XDG_CONFIG_HOME/chamber/config.toml` or `$HOME/.config/chamber/config.toml`
- root: `$XDG_DATA_HOME/chamber` or `$HOME/.local/share/chamber`
- run-root: `$XDG_RUNTIME_DIR/chamber`, falling back only to a user-private directory with strict ownership and permissions
- tmp-root: a Chamber-owned directory under the configured root or run-root, not a shared `/tmp/chamber`

At startup, the daemon should verify that configured paths are owned by the current user and are not group/world writable unless the exact path category requires it and the risk is understood. It should fail closed on unsafe storage, socket, lock, or runtime directory permissions.

Rootless support must account for:

- subuid/subgid availability
- user namespace setup
- cgroups v2 delegation
- rootless overlay support
- fallback storage drivers
- systemd user sessions
- rootless network backend availability

Per-user coexistence requirements:

- two users can start Chamber daemons on the same host at the same time;
- default sockets, pid files, locks, runtime bundles, temp paths, logs, and databases do not collide;
- image, snapshot, allowed mount, runtime, minimal network, and container identifiers are scoped to the daemon's state store;
- one user's garbage collector cannot inspect, lock, mark, or delete another user's Chamber data;
- helper processes inherit only Chamber-controlled per-user paths needed for the operation.

## Storage Strategy

Start with an existing storage library if possible. Storage correctness is one of the highest-risk parts of the project.

Chamber must provide strict storage control. Every persistent and temporary storage location must be known, configurable, and visible through diagnostics. This includes:

- image blobs
- manifests and metadata
- unpacked layers
- writable container root filesystems
- temporary pull files
- runtime bundles
- logs
- minimal network state
- sockets
- lock files
- garbage collection work directories

The daemon should create Chamber-owned temporary directories under its configured storage root instead of falling back to host defaults such as `/tmp` or `/var/tmp`. Any third-party library or helper process that uses temporary files must be wrapped or configured so its temp path is inside Chamber-controlled storage.

Storage paths must be scoped by user, either because they live under user-private XDG/home/runtime directories or because the configured path is explicitly owned by the current user with restrictive permissions. Chamber should not use a shared machine-wide image store, content store, lock directory, or runtime directory in the initial design.

The desired model:

- content-addressed blobs
- manifest/index metadata
- unpacked snapshot/rootfs records
- reference-counted or lease-protected usage
- transactional metadata updates
- resumable cleanup

Avoid a design where physical deletion is directly coupled to client-facing `remove` requests.

Storage diagnostics should answer:

- where every category of data is stored;
- which OS user owns the daemon and storage roots;
- how much space each category uses;
- which active leases are preventing cleanup;
- which operation created or currently owns temporary data;
- whether any configured library or helper is writing outside Chamber-managed paths.

## MVP

The first useful version should prove the controller-facing execution contract and concurrency model rather than feature breadth. It should answer one question: can Chamber reliably accept high-frequency automation requests for pull, run, logs, cancellation, deletion, recovery, and cleanup under one unprivileged user while coexisting with other users' Chamber daemons on the same machine?

MVP features:

- Start a rootless per-user node daemon from one binary.
- Expose Unix socket API.
- Pull an OCI image.
- Create and run a container through `crun` or `runc`.
- Capture logs and exit status.
- Cancel or stop an active operation/container.
- List images and containers.
- Remove containers.
- Mark images for deletion.
- Run lease-aware garbage collection.
- Survive daemon restart with coherent state.
- Handle concurrent pull/list/run/remove requests without corrupting state or returning avoidable race errors.
- Run concurrently beside another Chamber daemon on the same machine under a different OS user without shared mutable state or path collisions.
- Provide enough node status and event data for a future coordinator to make decisions.

MVP stress scenarios:

- 100 concurrent image list requests while pulling and deleting images.
- 50 concurrent runs from the same image.
- repeated pull/remove of the same tag while containers are running from the resolved digest.
- daemon crash during pull, container start, log capture, cancellation, and GC.
- two OS users running independent Chamber daemons with overlapping image names, container names, runtime settings, pulls, runs, and GC cycles.

Explicitly out of MVP:

- image build;
- image push;
- Docker, Podman, Compose, CRI, or Kubernetes compatibility;
- `ContainerExec`;
- general volume management;
- cross-container or cross-node networking;
- rich port publishing beyond what is needed for the selected automation workload;
- plugin systems or user-extensible runtime drivers.

## Deferred Features

These are intentionally tracked for later reference but should not shape the first release.

### Build Support

- Build OCI images from Containerfiles/Dockerfiles.
- Evaluate Buildah libraries.
- Evaluate BuildKit libraries, if usable without adopting containerd assumptions.
- Consider a constrained builder that delegates execution to Chamber containers.

Build support can expand scope dramatically and should only be added if the target automation workload cannot build images elsewhere.

### Image Push and Registry Write Operations

- Push images.
- Manage write-capable registry auth.
- Export OCI image layouts.

This should wait until pull/run reliability is proven.

### Rich Networking

- Localhost port publishing.
- Multiple network backends.
- Cross-container networking.
- Netavark-compatible integration, if practical.
- More advanced cleanup and recovery around network namespaces.

Port publishing is host-level even when Chamber state is user-scoped, so this needs careful conflict reporting across independent per-user daemons.

### Volumes and Mount Management

- Named volumes.
- Volume lifecycle APIs.
- More flexible bind mount policy.
- Mount cleanup and recovery beyond the minimum needed for container root filesystems.

### Exec and Attach

- `ContainerExec`
- interactive attach
- terminal resizing
- session lifecycle management

Logs and exit status are enough for the MVP controller contract.

### Compatibility APIs

- Docker-compatible API subset.
- Podman-compatible API subset.
- Compose support through Docker API compatibility.
- Kubernetes CRI compatibility.

Compatibility can consume the project if attempted before Chamber's own API and reliability model are proven.

### Coordinator-Facing Refinements

- richer admission policy;
- richer health models;
- resource pressure signals;
- advanced event filtering;
- higher-level retry hints.

## Key Design Questions

- Which storage library can support the desired transactional and lease-aware model?
- Should the daemon use SQLite, BoltDB, Badger, or another embedded state store?
- Should physical image/layer storage be managed by `containers/storage`, an OCI layout store, or a custom content store?
- Can every chosen library be forced to use Chamber-controlled persistent and temporary directories?
- What should Chamber do if a helper process cannot guarantee controlled temp paths?
- What exact default path strategy should Chamber use when `$XDG_RUNTIME_DIR` is missing or unusable?
- Which ownership and permission checks should be mandatory before opening the state database, socket, lock files, runtime bundles, and temporary roots?
- How should diagnostics represent the daemon identity: machine ID, OS username, UID/GID, configured roots, and socket path?
- Is `crun` the default runtime because of rootless performance and cgroup behavior?
- Is MVP networking required at all for the first target workload, or can it start with no network or outbound-only networking?
- If outbound networking is required, what rootless backend gives the best reliability for that narrow use case?
- What node status, operation, event, and health APIs will a future coordinator need?
- Which responsibilities must stay above Chamber in the distributed control plane?
- What concrete consumer workload will prove Chamber is better than shelling out to Docker or Podman?

## Risks

- Storage correctness is hard and easy to underestimate.
- Rootless networking has host-specific behavior and rough edges.
- A future distributed control plane may pull too many orchestration concerns into the node daemon if the boundary is not explicit.
- General-purpose container engine scope is much larger than the controller-facing execution daemon scope.
- Existing libraries may expose lower-level assumptions that fight the daemon's desired concurrency model.
- Third-party libraries and helper tools may write to implicit temp locations unless carefully configured.
- A library or helper may assume machine-wide defaults for sockets, locks, temp files, network names, or runtime directories, causing cross-user collisions unless wrapped carefully.
- Port publishing is inherently host-level even when daemon state is user-scoped; Chamber must report conflicts clearly without treating another user's daemon as shared state.
- Deferred features may feel natural to add early; each one should require a concrete workload and a fresh scope decision.

## Success Criteria

Chamber is successful if it can:

- run rootless without sudo;
- act as a reliable node runtime for a distributed automation platform;
- accept many concurrent client and coordinator requests through one daemon;
- run beside another Chamber daemon on the same machine under a different OS user without shared mutable state, socket collisions, lock collisions, temp collisions, or cleanup interference;
- avoid destructive races through leases and operation coordination;
- run OCI containers through existing runtimes;
- pull images and create, run, stop, list, log, and remove containers;
- keep all persistent and temporary storage inside Chamber-controlled locations;
- recover cleanly after daemon crashes;
- provide a node API usable by local tools and a future distributed coordinator.
