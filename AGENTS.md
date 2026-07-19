# Chamber Agent Memory

## What Chamber Is

Chamber is becoming two closely related products built from the same Go implementation:

1. **Embeddable SDK primitives** for point-and-shoot container work.
   These packages should let a caller pull an OCI image into a caller-chosen root, unpack/provision a runtime bundle, ensure a runtime binary, run a bundle, and read logs or results. SDK callers own storage placement, concurrency, cleanup, cancellation policy, and recovery unless a specific package says otherwise.

2. **`chamberd`, a local execution daemon** that composes those same primitives into a reliable node-local authority.
   The daemon owns durable operation records, leases, state transitions, concurrency control, cancellation, logs, recovery, and cleanup for one configured authority domain.

The daemon should be the first serious user of the Go SDK. If `chamberd` cannot be built naturally from the public Go packages, the SDK boundary is probably wrong.

Chamber is still the node-local execution substrate, not a distributed scheduler or a general-purpose container engine. Distributed placement, cluster membership, desired-state reconciliation, secrets distribution, fleet-level policy, and global queues belong above Chamber.

## Product Boundaries

The SDK layer is deliberately lower-level than the daemon:

- It performs explicit operations against caller-provided locations.
- It should be useful from Go directly and later from other languages through thin wrappers, likely around a small Go-built helper binary rather than duplicated logic.
- It should not quietly promise daemon-grade safety. Shared roots, locking, cleanup, and crash recovery are caller responsibilities unless explicitly delegated to `chamberd`.
- Public SDK packages live under `pkg/`, with stable user-facing types for the reusable image, bundle, runtime, and shared filesystem primitives.

The daemon layer adds the reliability contract:

- One daemon is the single authority over its mutable node-local state.
- Concurrent pull, run, list, remove, cancel, log, and GC requests are expected.
- Mutating operations should have durable operation records and explicit state transitions.
- Destructive work must be lease-aware and recoverable.
- The CLI and other clients should talk to the daemon API; they should not read or mutate daemon storage directly.

## Privilege Model

Rootless remains the default and first-class mode. Normal Chamber operation should not require `sudo`.

Rootful support may be added as a configurable privilege mode later, but treat it as a meaningful expansion of the trust boundary, especially for `chamberd`. A rootful daemon serving non-root clients is a local privilege boundary and needs explicit authorization, peer credential checks, allowed operations, and careful auditability.

Model privilege cleanly:

- Top-level daemon/runtime privilege is probably `rootless` or `rootful`.
- `chamberd` config should own one top-level privilege setting and project it
  into SDK adapter configs during composition; do not let bundle and runtime
  config drift into independently selected daemon privilege modes.
- Details such as user namespaces, cgroups, networking, capabilities, and seccomp are isolation/runtime profile settings, not necessarily additional top-level privilege modes.
- Rootful-with-user-namespace is real and useful, but should be represented as rootful privilege plus a user-namespace profile rather than as a vague third mode.

## Core Architecture Priorities

- Reuse proven OCI/container libraries and runtimes where practical instead of reimplementing low-level container primitives.
- Keep persistent and temporary storage inside Chamber-controlled or caller-provided roots; avoid broad reliance on ambient host defaults.
- Make path ownership explicit. Multiple OS users on the same host must not accidentally share sockets, storage, locks, runtime directories, temp directories, logs, names, network bookkeeping, or cleanup authority.
- Start with narrow, testable packages and invariants before widening into compatibility APIs.
- Keep code fixes and new implementation as minimal as possible without playing code golf. Prefer the smallest clear change that preserves the contract, tests the behavior at risk, and does not hide complexity behind clever compression.
- Prefer explicit top-down dependency injection for filesystem, runtime, and backend choices.
- Keep package-owned config at generic boundaries. Avoid letting global config import concrete adapters such as specific metadata or runtime implementations.
- Keep shared helpers small and policy-shaped. `pkg/shared/localfs` should encode private-directory and temp-file policy, not become a broad utility grab bag.

## Naming And Import Conventions

- If a Chamber package import needs an alias, use a readable `chamber...` alias rather than a shortened `ch...` alias.
  Examples: `chamberErrors`, `chamberBundle`, `chamberImage`, `chamberRuntime`, `chamberDaemonConfig`.
- When importing a Chamber package that is a concrete adapter around a third-party implementation, make the alias say it is the Chamber adapter, not the upstream project.
  Example: use `chamberDirectoryProvisioner` for `github.com/donglin-wang/chamber/pkg/bundle/directory` rather than `umoci` at composition boundaries.
- Prefer specific adapter aliases at daemon composition boundaries when they clarify the role:
  `chamberImagePuller`, `chamberRuncRuntime`, and `chamberEtcdMetadataStore`.
- `localfs.DirectoryManager` values should be named `directoryManager` or a similarly explicit name. Do not shorten them to `directories`; that sounds like a collection of paths rather than the filesystem policy dependency.
- Concrete Chamber adapter packages should be named for the Chamber role they implement, not for the third-party library they currently use. Mention the backing library in the package comment, type comment, and implementation docs.
- Keep upstream third-party imports visually distinct from Chamber adapter packages. For example, inside `pkg/bundle/directory`, alias the upstream `github.com/opencontainers/umoci` import as `ociumoci` so readers can tell it apart from Chamber's directory-backed OCI adapter package.

## Current Implementation Shape

The current repo is still early and may not yet have the final public SDK layout.

Important current boundaries:

- `pkg/image`: public puller contract, pull request platform/auth fields, image-root config, and small layout helpers. Configured image roots are the source of truth for pulled-image storage; concrete pullers derive destinations from canonical image references. SDK callers own root placement, locking, cleanup, and recovery.
- `pkg/image/puller`: concrete OCI image puller using `go-containerregistry`. It honors platform/auth request fields and must keep the atomic temp-then-rename layout write behavior. Future sibling implementation packages may include `pkg/image/pusher` and `pkg/image/inspector`.
- `pkg/bundle`: public bundle provisioning contract, bundle-root config, `ProcessSpec`, and `RootFS.Mounts`. `RootFS.Mounts` is intentionally ahead of runtime support as a future overlayfs/snapshot hook.
- `pkg/bundle/directory`: concrete directory-backed OCI bundle provisioner using `umoci`. It currently supports rootless provisioning and owns unpacking, rootless OCI spec patching, private `config.json` writes, temporary staging, and atomic final bundle placement.
- `pkg/runtime`: public runtime contract and runtime config. The shared `runtime.New` constructor owns shared validation, including Linux host gating, implementation name/capability checks, and private runtime directory creation, then dispatches to the registered concrete implementation constructor. Concrete runtime constructors own implementation-specific initialization such as binary verification/download and runtime-owned log handling. The `Runtime` interface includes `Descriptor`, `Binary`, `Run`, `State`, `Signal`, `Delete`, and `ReadLog`. `Run` returns only a `Process`; it must not pretend to return current container lifecycle state. Use `State` for actual runtime state.
- `pkg/runtime/runc`: concrete `runc` adapter, including runtime binary ensure/download, state/signal/delete calls, and runtime-owned log handling. It must continue rejecting non-empty `RootFS.Mounts` until mount application exists.
- `pkg/shared/errors`: canonical public Chamber error-code taxonomy. The daemon and SDK adapters should use these durable codes for contract errors and response mapping.
- `pkg/shared/containerid`: shared container ID validation used by provisioning and runtime adapters so bundle creation cannot accept IDs the runtime later rejects.
- `pkg/shared/localfs`: explicit filesystem dependency for private directories and temp files. It owns filesystem policy primitives, not broad utility behavior.
- `pkg/shared/testutil`: shared tests helpers. Its location is not ideal, but keep it there for now.
- `daemon/metadata`: daemon-owned durable vocabulary for images, containers, operations, and states. Keep daemon-only sentinel storage errors such as `ErrNotFound` and `ErrAlreadyExists` here, but use `pkg/shared/errors.Code` for durable operation/container error codes.
- `daemon`: current daemon HTTP composition and operation orchestration.

Future public Go SDK work should build on these `pkg/` contracts without moving daemon reliability concepts into the SDK. Candidate future surfaces include a convenience one-shot run package.

Boundary rules for future changes:

- Do not add build or push support until the pull/provision/run contracts are solid.
- Do not move daemon reliability concepts such as durable operations, leases, recovery, cancellation policy, or garbage collection into the low-level SDK packages.
- Do not make global daemon config import concrete adapters. Global config should depend on generic package config types; composition chooses concrete implementations.
- Do not let the daemon guess SDK-owned paths. Persist paths returned by SDK operations, such as `ProvisionedBundle.BundlePath`, and call runtime-owned APIs such as `ReadLog`.
- Keep rootless defaults first-class. Rootful support should be introduced as an explicit privilege-boundary expansion, not as a quiet option on existing APIs.

## Review And Cleanup Passes

Use review passes to make the code easier to understand without changing its contract. A review pass is not permission to redesign the system, add broad new abstractions, move files around, or widen scope.

When asked for a code review, lead with correctness and boundary findings:

- Code smells that can hide bugs, especially duplicated policy, unclear ownership, implicit global state, over-broad helpers, and state that can drift between packages.
- Abstraction and package-boundary oversteps, especially daemon reliability concepts leaking into `pkg/`, concrete adapters leaking into generic config, or helper packages becoming grab bags.
- Missing tests around contract boundaries, storage invariants, platform gates, error mapping, idempotency, and race-prone operations.
- Long-term API shape issues such as names, fields, or interfaces that suggest a stronger guarantee than the implementation actually provides.

When asked to clean up or simplify code:

- Look for "helper for helper" chains: private helpers that are used once, merely forward to another helper, or make the call path harder to read without hiding real complexity. Inline them when doing so makes the owning function clearer.
- Look for structs, config types, interfaces, or fields with dual responsibilities. A field should not both select an implementation and describe implementation-private behavior; an interface should not include lifecycle/setup methods if constructors already return ready values.
- If a file is roughly 500 lines or longer, scan it for obvious simplifications. Prefer small local simplifications inside the file. Do not create a new abstraction or a new file merely because the file is long.
- Preserve behavior unless the user explicitly asked for a behavior change. Keep diffs small, testable, and easy to review.
- Leave code alone when simplification is not obvious. A boring, explicit implementation is better than a clever cleanup that moves complexity somewhere else.
- Do not commit during review or cleanup unless the user explicitly asks for a commit.

## Daemon Contract

`chamberd` should make one local authority dependable. It should expose a stable local API over a user-scoped Unix socket by default, with TCP/HTTP only as an explicit demo or development option.

The daemon should support:

- image pull/list/remove;
- container create/start/run/stop/remove/list;
- logs;
- operation status/cancel;
- system info/events;
- crash recovery;
- lease-aware garbage collection.

Use operation-scoped locks rather than a single global lock where possible: image reference locks, content-object locks, container locks, runtime/network allocation locks, and a global GC coordination lock only where needed.

## Non-Goals For Now

- Do not depend on containerd.
- Do not build a Kubernetes replacement or distributed scheduler inside Chamber.
- Do not target Docker, Podman, Compose, Kubernetes CRI, image build, or image push until the core contract is proven.
- Do not add rich networking, general volumes, or plugin systems before the execution and recovery model is solid.
- Do not treat cleanup as ad hoc deletion. Cleanup must be lease-aware in the daemon, and explicitly caller-owned in the SDK.

## Learning Preferences

This project is also a learning project for the repository owner. Future agents should optimize for teaching and collaboration while still being free to write as much code as the task calls for.

The owner wants to gain:

- Fundamentals of container runtimes, especially rootless execution, OCI images/specs/runtimes, storage, namespaces, cgroups, leases, cleanup, and recovery.
- Muscle memory writing Go.
- Good engineering judgment around package boundaries, naming, config layering, and filesystem ownership.

Default collaboration style:

- Prefer explaining the design pressure, tradeoffs, and next small implementation step.
- When implementation is needed, write the code required to complete the task; there is no standing limit on how much code an agent may write.
- Keep changes focused and reviewable when practical, but do not withhold a full implementation merely to preserve a teaching exercise.
- Invite the owner to make core design and coding decisions themselves.
- Provide Go sketches, interfaces, tests, pseudocode, or full implementations depending on what best serves the request.
- Use questions that force useful thinking, but do not block on questions when a conservative local assumption is obvious.

Good agent behavior for this repo:

- Start with research spikes, invariants, small Go packages, and tests.
- Explain container-runtime concepts in the context of the code being written.
- Review owner-written code carefully for correctness, race conditions, error handling, permissions, and recovery behavior.
- Keep docs aligned with code when moving package boundaries.

Avoid by default:

- Hiding important Go or container-runtime details behind too much abstraction.
- Exposing unstable internal packages as public SDK just because they are convenient today.
- Letting the SDK and daemon contracts blur together.
- Adding broad container-engine features before the core SDK primitives and daemon reliability contract are proven.
