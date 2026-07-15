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
- Details such as user namespaces, cgroups, networking, capabilities, and seccomp are isolation/runtime profile settings, not necessarily additional top-level privilege modes.
- Rootful-with-user-namespace is real and useful, but should be represented as rootful privilege plus a user-namespace profile rather than as a vague third mode.

## Core Architecture Priorities

- Reuse proven OCI/container libraries and runtimes where practical instead of reimplementing low-level container primitives.
- Keep persistent and temporary storage inside Chamber-controlled or caller-provided roots; avoid broad reliance on ambient host defaults.
- Make path ownership explicit. Multiple OS users on the same host must not accidentally share sockets, storage, locks, runtime directories, temp directories, logs, names, network bookkeeping, or cleanup authority.
- Start with narrow, testable packages and invariants before widening into compatibility APIs.
- Prefer explicit top-down dependency injection for filesystem, runtime, and backend choices.
- Keep package-owned config at generic boundaries. Avoid letting global config import concrete adapters such as specific metadata or runtime implementations.
- Keep shared helpers small and policy-shaped. `pkg/shared/localfs` should encode private-directory and temp-file policy, not become a broad utility grab bag.

## Current Implementation Shape

The current repo is still early and may not yet have the final public SDK layout.

Important current boundaries:

- `pkg/image`: puller contract and image-root config.
- `pkg/image/gocontainerregistry`: concrete OCI image puller.
- `pkg/bundle`: bundle provisioning contract and bundle-root config.
- `pkg/bundle/umoci`: concrete bundle provisioner using `umoci`.
- `pkg/runtime`: runtime contract and runtime config.
- `pkg/runtime/runc`: concrete `runc` adapter, including runtime binary ensure/download and log handling.
- `pkg/shared/localfs`: explicit filesystem dependency for private directories and temp files.
- `daemon/metadata`: daemon-owned durable vocabulary for images, containers, operations, states, and errors.
- `daemon`: current daemon HTTP composition and operation orchestration.

Future public Go SDK work should build on these `pkg/` contracts without moving daemon reliability concepts into the SDK. Candidate future surfaces include a convenience one-shot run package.

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
