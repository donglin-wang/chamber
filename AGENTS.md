# Chamber Project Guidance

## Direction and Ownership

Chamber is a Go SDK for simple, ergonomic OCI container execution in embedded
applications, automation, and CI pipelines. Linux is the supported execution
target. Rootless execution is the default and normal operation should not need
`sudo`.

The common path is pull or build an `image.Image`, construct a runtime, call
`Runtime.RunImage`, then wait and delete the container. Returned images carry
the local OCI layout path, reference, digest, and platform so execution needs no
store lookup or manual provisioner setup.

- The image store owns its shared OCI layout and image metadata. Returned layout
  paths are populated from the live store location, including cached pulls and
  listing results; do not persist absolute layout paths in image metadata.
- `RunImage` owns bundle provisioning and the provisioning workspace under the
  runtime root. Launch failures release the provisioned bundle. The returned
  container owns subsequent bundle cleanup through `Delete`.
- `Run` accepts a ready OCI bundle. That bundle remains caller-owned.
- Callers own root placement, coordination of shared roots and IDs, application
  cancellation policy, cleanup after crashes, and image/cache retention.
- Do not add shared-root concurrency management, operation records, leases,
  recovery controllers, distributed scheduling, or background services to the
  SDK. Application-level policies belong in embedding applications.
- `cmd/ci` is an SDK consumer and automation example. Its webhook
  admission limits, per-run roots, and retention are application policy.

## Linux and Host Execution

- Design and validate namespaces, process death signals, runc, RootlessKit,
  BuildKit, sockets, ownership, and cleanup for Linux only.
- Do not add non-Linux compatibility shims or no-op fallbacks to keep native
  macOS green. Use explicit `unsupported_host` errors at public construction or
  preflight boundaries where useful.
- Native macOS may be used for editing and cross-compilation. Runtime evidence
  must come from Linux or Lima; label compile-only validation accurately.
- All process spawning under `pkg/` must use Chamber's shared subprocess helper
  for Linux `Pdeathsig` protection. Flag raw `exec.Command`/`CommandContext`
  unless there is an explicit documented reason to opt out.
- Use `hostfs.Workspace` for managed binary directories and download staging,
  following the BuildKit and runtime installers. Keep pinned checksums, cached
  binary verification, and atomic installation. The directory provisioner's umoci
  helper lives in its workspace's `.bin` directory, which cannot collide with a
  valid bundle ID. Keep its executable version aligned with the umoci Go library.
- Rootful support is a future privilege-boundary expansion, not a quiet option
  on existing APIs. User namespaces, cgroups, networking, capabilities, and
  seccomp are isolation settings rather than extra privilege modes.

## Package Shape

Public SDK packages live under `pkg/`:

- Root packages such as `pkg/image`, `pkg/bundle`, and `pkg/runtime` own public
  contracts, configs, schemas, fixed string constants, and small domain helpers.
- Factory packages own ready-object constructors such as `NewStore`,
  `NewProvisioner`, and `NewRuntime`. They validate shared config, check Linux
  host requirements, create private workspaces, and select implementations.
- Concrete implementations live under `pkg/<package>/internal/<implementation>`.
  They use the root contracts and expose ordinary `New` constructors for their
  factory. Keep shared construction policy in factories.
- If multiple implementations become necessary, put their names in root
  contracts/config and the selection list in the factory. Do not add registry,
  `init`, side-effect-import, or plugin systems.
- SDK configs are final configs. Callers resolve files, environment, defaults,
  and flags before construction. Do not add SDK `Override` or `Resolve` layers.

Current boundaries:

- `pkg/image`: pull/build/list/remove contracts, image references and platforms,
  image metadata, layout validation, and BuildKit configuration. The store
  writes shared layout content before metadata. `PullIfMissing` reuses valid
  records; `PullAlways` refreshes content while preserving `CreatedAt`.
- `pkg/image/internal/store`, `metadata`, `registry`, and `buildkit`: filesystem
  store orchestration, JSON records, registry pulls via go-containerregistry,
  and rootless Dockerfile builds via BuildKit. Tool state stays under the image
  root or its configured temporary root.
- `pkg/bundle`: provisioner contracts, config, `ProcessSpec`, mounts, and
  `ProvisionedBundle`. Bundle IDs identify provisioner-owned storage, independent
  of runtime container IDs. Retain the returned ID and path for removal.
- `pkg/bundle/internal/directory`: directory-backed OCI provisioning using
  umoci, rootless spec patching, staging, private spec writes, atomic placement,
  and removal of provisioner-owned resources.
- `pkg/runtime`: `RunImage` and ready-bundle `Run`, config, container lifecycle,
  logs, and results. The runtime factory composes the provisioner with the
  runtime adapter. Runtime-created bundles belong below the runtime root.
  Launch contexts govern launch only; returned containers own lifecycle.
  `Wait` observes completion and releases launch resources; `Delete` removes
  runtime state and any bundle owned by `RunImage`. Logs are retained until
  `DeleteLog`. Keep caller-provided bundles untouched.
- `pkg/shared/errors`: stable public error codes. Choose errors at the failing
  Chamber step instead of classifying upstream sentinels or adding wrappers
  solely to preserve nested codes. Use precise validation codes for image
  layouts/references, mounts, process specs, and container IDs.
- `pkg/shared/containerid`: runtime container ID validation. Bundle IDs are
  validated independently in `pkg/bundle`.
- `pkg/shared/hostfs`: scoped private directories, temporary roots, private
  files, and filesystem capability policy. Keep it out of broad utility work.
- `pkg/shared/testutil`: shared test helpers; leave this location as-is for now.

Prefer proven OCI libraries and runtimes over rebuilding low-level primitives.
Do not add containerd, distributed scheduling, rich networking, general volumes,
or compatibility layers before core execution contracts are solid.

## Clarity, Naming, and Refactors

Proactively simplify changes around resource ownership and existing code. Put
standard defaults in the package that creates the resource: bundle provisioning
owns generated OCI spec defaults, while `Runtime.Run` executes caller-provided
bundles unchanged and callers own externally prepared specs. Keep standard
behavior automatic without per-caller switches. Remove adaptation layers made
unnecessary by that placement, including spec copies, path rewriting, temporary
artifacts, cleanup branches, and obsolete helpers or tests. Put SDK integration
coverage in the owning SDK package; consumer tests should cover application
behavior. Extend existing test files for related behavior, creating a separate
file only when it provides a meaningful organizational boundary. Make these
simplifications without waiting for a prompt, preserve useful coverage and
ownership contracts, and aim for a clear net reduction in code and moving parts.

- Keep changes as small and explicit as the contract permits. A seasoned Go
  reader should understand process stages, ownership, and side effects from
  names and nearby control flow.
- Every helper should name a domain step, isolate a hazardous invariant, hide
  meaningful complexity, or provide a necessary test seam. Inline helpers that
  merely forward, wrap literals, or obscure control flow.
- Prefer behavior names such as `createRunDirectories` or
  `requireSubordinateIDMapping` over vague `prepare`, `handle`, or `normalize`.
- Scan files around 500 lines or longer for obvious local simplifications.
  Avoid creating files or abstractions just to shorten long files.
- Before moving a type, field, helper, schema, or durable file, identify its
  owner, which package may persist/mutate it, and which callers need to know it.
- Use one canonical type for a concept. Persistence envelopes may add package-
  owned versioning, but do not duplicate event/request/result schemas.
- Do not introduce single-implementation interfaces without a real boundary or
  test seam. Prefer concrete types or a function type for one callback.
- Do not expose both a package function and an interface method for the same
  behavior. Names must not promise stronger guarantees than the implementation.
- Use readable `chamber...` import aliases, such as `chamberImage`,
  `chamberBundleFactory`, and `chamberRuntime`; avoid shortened `ch...` names.
- Name concrete adapters for Chamber's role, with the upstream library in
  comments. Keep adapters internal and upstream imports visually distinct;
  e.g. `ociumoci` inside the directory provisioner.
- Composition should use root contracts and factories, with concrete adapters
  referenced only in factories or implementation-specific tests.
- Name workspaces for their owner: `imageWorkspace`, `bundleWorkspace`,
  `runtimeWorkspace`.
- Use standard `os.Signal`/`syscall.Signal` for signals and fixed runtime string
  types/constants for public statuses and log streams.

## Reviews and Validation

Lead reviews with correctness, ownership, boundary violations, and missing
contract coverage. Flag duplicated policy, implicit global state, drifting
configuration, misleading guarantees, storage invariant failures, unsafe
cleanup, and error mapping. Cleanup passes preserve behavior and avoid broad
redesigns. Do not commit unless explicitly requested.

For public API changes, audit every affected caller in the same turn:

- Search with `rg` for changed and removed types, fields, methods, and names.
- Compile affected downstream packages using
  `GOCACHE=/tmp/chamber-go-cache GOOS=linux go test -c`.
- Include `cmd/ci` for changes to runtime, bundle, image, process
  environment, logs/results, filesystem roots, or host execution.
- Include relevant factories and concrete adapters for root SDK changes.
- Update every result-field caller when a field's meaning or type changes.
- Run meaningful tests on Linux or Lima and inspect runtime artifacts when
  validating actual execution. Distinguish source/compile checks from runtime
  tests and deterministic lifecycle cases from concurrency stress testing.

## Collaboration and Learning

The owner wants container-runtime fundamentals, Go muscle memory, and sound
judgment around config, names, package boundaries, filesystem ownership, and
cleanup. Explain design pressure and tradeoffs where useful, and write the full
implementation needed to complete the requested work. Keep MVP code focused and
understandable with names and boundaries that can grow. Invite core decisions
without blocking obvious conservative local choices. Avoid hiding important
Go or container details behind unnecessary abstractions.
