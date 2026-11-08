# Contributing to Chamber

Keep changes focused, explain meaningful tradeoffs, and include validation
results in pull requests.
See the [README](README.md) for setup and a runnable example.

## Scope and ownership

Chamber is a Linux-only Go SDK for rootless OCI execution. Application policy,
including shared-root coordination, cancellation, crash recovery, and cache
retention, belongs to callers. Keep CI orchestration in the existing CI manifests.

- Create Chamber-managed files and directories through `pkg/shared/hostfs`
  wherever possible. Reuse the owning package's `hostfs.Workspace` for persistent
  and temporary files, directories, and staging so path scoping, private
  permissions, and ownership checks stay consistent. Prefer these helpers over
  direct `os.Mkdir*`, `os.Create*`, or `os.WriteFile` calls. Use direct `os`
  creation only when `hostfs` cannot express the required operation, and document
  the reason.
- The image store owns its OCI layout and metadata. Write content before metadata;
  populate returned layout paths from the live store rather than persisting them.
- `RunImage` owns provisioning under the runtime root. Release bundles on launch
  failure; after launch, the container owns their cleanup through `Delete`.
- `Run` executes caller-owned bundles unchanged. Generated OCI defaults belong in
  bundle provisioning, and bundle IDs are independent of runtime container IDs.
- Launch contexts govern launch only. `Wait` observes completion and releases
  launch resources; `Delete` removes runtime state and owned bundles. Logs remain
  until `DeleteLog`.

## Package boundaries

- Root packages (`pkg/image`, `pkg/bundle`, `pkg/runtime`) own public contracts,
  configs, constants, and domain types.
- Factories validate config and host requirements, create private workspaces, and
  select implementations under `pkg/<package>/internal/<implementation>`.
- Configs are final inputs: callers resolve environment, files, and flags.
- Use stable codes from `pkg/shared/errors` at the failing Chamber step.
  Keep `pkg/shared/hostfs` focused on filesystem ownership and capability policy.

Prefer established OCI libraries and runtimes. Avoid plugin registries,
background services, compatibility layers, and abstractions without a concrete
need. Keep rootless privilege boundaries explicit.

## Implementation style

- Keep ownership, process stages, and cleanup visible in nearby control flow.
  Put defaults in the package that creates the resource and remove redundant
  adapters when ownership makes them unnecessary.
- Use one canonical type per concept. Add helpers and interfaces only for a
  meaningful domain step, invariant, complexity boundary, or test seam.
- Use descriptive behavior names and readable `chamber...` import aliases.
  Name workspaces for their owner, such as `runtimeWorkspace`.
- Use `pkg/shared/logging` for SDK host logs. Put the event message or actual
  subprocess output line in `msg`, without a separate `line` field. Each logging
  implementation defines a short `logSource` string constant in its own package,
  using `<package>/<implementation>` (for example, `image/buildkit`), and includes
  it as `source` on every log, including bridged library logs. Omit redundant
  implementation or fixed library labels such as `runtime: runc` or `library: umoci`.
  Keep subprocess `process` and `stream` as separate fields when they distinguish
  output within an implementation, such as BuildKit's `buildkitd` and `buildctl`.
  Use specific origin fields such as `image_source` and `binary_source` so `source`
  always identifies the implementation.
- All process spawning under `pkg/` uses the shared subprocess helper for Linux
  `Pdeathsig` protection unless an explicit reason for opting out is documented.
- Use `hostfs.Workspace` for managed binaries and download staging. Preserve pinned
  checksums, cached binary verification, private staging, and atomic installation.
  Keep the umoci helper version aligned with the Go library.
- Keep SDK integration coverage in the owning package and extend related test
  files. Consumer tests cover application behavior. Format Go changes with `gofmt`.

## Validation and review

Run relevant tests on a rootless-capable Linux host or Lima VM:

```sh
GOCACHE=/tmp/chamber-go-cache go test ./...
GOCACHE=/tmp/chamber-go-cache go vet ./...
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 -timeout=20m ./pkg/...
```

`go run ./cmd/local-test` builds `Dockerfile.test` and runs the SDK suite in a
container, including nested rootless execution. CircleCI uses this runner.

For public API changes, search for affected names and update all callers and docs.
Compile affected downstream packages with `GOCACHE=/tmp/chamber-go-cache
GOOS=linux go test -c -o /tmp/<unique-name>.test <package>`, including relevant
factories, concrete implementations, and `cmd/local-test`. Native macOS supports
editing and cross-compilation; report those checks separately from Linux runtime
validation. Do not add macOS runtime shims or no-op fallbacks.

Review correctness, resource ownership, cleanup, storage invariants, error codes,
and contract coverage. Describe behavior changes, tests run, and remaining limits
in the pull request. Preserve behavior during cleanup and keep redesigns separate.
