# Chamber Agent Memory

## Project Context

Chamber is a rootless, per-user execution daemon for distributed automation platforms. It should provide one local daemon per OS user that accepts same-user client or coordinator requests over a Unix socket and coordinates image pull, container execution, logs, cancellation, state, recovery, and cleanup without requiring `sudo`.

Chamber is the node-local execution substrate, not a distributed scheduler or a general-purpose container engine. Preserve the boundary: distributed placement, cluster membership, desired-state reconciliation, secrets distribution, and fleet-level policy belong above Chamber.

Key design priorities from `plan.md`:

- Rootless by default.
- One daemon is the single authority over one user's mutable node-local state.
- Multiple OS users on the same host must have independent sockets, storage, locks, runtime directories, temp directories, logs, names, network bookkeeping, and garbage collection authority.
- Concurrency is a product feature: overlapping pull, run, list, remove, cancel, log, and GC requests should be expected.
- Use durable operation records, explicit state transitions, operation-scoped locks, and leases to avoid destructive races.
- Reuse proven OCI/container libraries and runtimes where practical instead of reimplementing low-level container primitives.
- Keep all persistent and temporary storage inside Chamber-controlled, user-private paths.
- First prove the controller-facing reliability contract before adding compatibility APIs or broad container-engine features.

## Learning Preferences

This project is also a learning project for the repository owner. Future agents should optimize for teaching and collaboration while still being free to write as much code as the task calls for.

The owner wants to gain:

- Fundamentals of container runtimes, especially rootless execution, OCI images/specs/runtimes, storage, namespaces, cgroups, leases, cleanup, and recovery.
- Muscle memory writing Go.

Default collaboration style:

- Prefer explaining the design pressure, tradeoffs, and next small implementation step.
- When implementation is needed, write the code required to complete the task; there is no standing limit on how much code an agent may write.
- Keep changes focused and reviewable when practical, but do not withhold a full implementation merely to preserve a teaching exercise.
- Invite the owner to make core design and coding decisions themselves.
- Provide Go sketches, interfaces, tests, pseudocode, or full implementations depending on what best serves the request.
- Use questions that force useful thinking, but do not block on questions when a conservative local assumption is obvious.
- Point to the relevant part of `plan.md` when making architecture decisions.

Good agent behavior for this repo:

- Start with research spikes, invariants, small Go packages, and tests.
- Explain container-runtime concepts in the context of the code being written.
- Review owner-written code carefully for correctness, race conditions, error handling, permissions, and recovery behavior.

Avoid by default:

- Hiding important Go or container-runtime details behind too much abstraction.
- Adding Docker, Podman, Compose, Kubernetes, build, push, rich networking, general volumes, or plugin support before the MVP contract is proven.
- Treating cleanup as ad hoc deletion; cleanup must be lease-aware and recoverable.
