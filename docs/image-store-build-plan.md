# Image Store And Dockerfile Build Plan

Historical note: this plan predates the `pkg/shared/hostfs.Workspace`
migration. Any references to `localfs.DirectoryManager` or old factory
signatures are superseded by scoped host filesystem workspaces.

This plan describes a focused refactor of `pkg/image` so image storage is owned by a single public `image.Store` interface. It intentionally ignores backward compatibility with the current public `image.Puller` interface.

## Context

Chamber currently has:

- Public image contracts in `pkg/image`.
- Public factory constructors in `pkg/image/factory`.
- A concrete registry pull implementation in `pkg/image/internal/puller`.
- Pulled OCI image layouts stored under `image.Config.Root`.

The new direction is:

- `image.Store` is the only public image-operation interface.
- Pulling and building are methods on the store.
- Result structs are records, not lifecycle-owning objects. Use one public `Image` record for images stored by either pull or build.
- One image store owns one canonical shared OCI image layout directory.
- `Image` does not contain a layout path. The store exposes the shared layout path through `Layout(ctx)`.
- The image package owns filesystem storage mechanics.
- The SDK caller or `chamberd` owns the cleanup decision.

This follows the same responsibility split as container image systems such as containerd: storage plugins/stores own physical storage mechanics, while higher layers own reachability, leases, policy, and garbage-collection decisions.

## Implementation Guidelines

- Avoid helper-of-helper chains. Extract a function only when it lets the reader see a concrete logic step in the process, such as validating a selected descriptor, committing blobs, rewriting the layout index, or writing metadata atomically. Do not create helpers just to make functions shorter.
- Choose names that make the process feel self-evident from first principles. A reader should be able to follow the image flow as concrete steps: fetch or build content, validate the temp layout, copy blobs by digest, rewrite the shared index, write metadata, and clean temp state.
- Prefer boring explicit code at the storage boundary. Atomic filesystem operations, OCI layout mutation, and metadata writes are places where clarity matters more than clever abstraction.

## Verdict On Naming

Do not rename `Puller` to `Store`.

Instead:

- Delete the public `image.Puller` interface.
- Introduce public `image.Store`.
- Keep operation-specific implementation helpers private.

Use this private package split:

```text
pkg/image/internal/store
pkg/image/internal/metadata
pkg/image/internal/registry
pkg/image/internal/buildkit
```

Rationale:

- `store` orchestrates pull/build/list/remove/layout operations and owns shared OCI layout mutation.
- `metadata` owns durable local image records, metadata key derivation, atomic JSON writes, reads, lists, deletes, and corruption handling.
- `registry` owns remote registry pull mechanics. It may later grow push/auth/token helpers, so it should not be named `pull`.
- `buildkit` owns Dockerfile build mechanics through BuildKit. BuildKit is the build engine/backend, so the package name should be explicit.

## Target Public API

Update `pkg/image/contract.go` around the existing image contracts.

```go
type Store interface {
    Pull(ctx context.Context, request PullRequest) (Image, error)
    Build(ctx context.Context, request BuildRequest) (Image, error)
    List(ctx context.Context, request ListRequest) ([]Image, error)
    Remove(ctx context.Context, request RemoveRequest) error
    Layout(ctx context.Context) (string, error)
}
```

Replace operation-specific public result types such as `PulledImage`, `BuiltImage`, and `StoredImage` with one `Image` record:

```go
type Image struct {
    Reference  string
    Digest     string
    Platform   Platform
    Source     Source
    SizeBytes  int64
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type Source string

const (
    SourcePulled Source = "pulled"
    SourceBuilt  Source = "built"
)
```

Add request types:

```go
type BuildRequest struct {
    Reference      string
    ContextPath    string
    DockerfilePath string
    Platform       Platform
    Target         string
    BuildArgs      map[string]string
}

type ListRequest struct {
    Reference string
}

type RemoveRequest struct {
    Reference string
    Platform  Platform
}
```

Extend image config for BuildKit-backed builds:

```go
type Config struct {
    Root     string
    BuildKit BuildKitConfig
    Logging  logging.Config
}

type BuildKitConfig struct {
    // BuildctlPath is the absolute path to the buildctl executable used by
    // Store.Build. Empty keeps pull/list/remove usable but makes Build return an
    // invalid request error.
    BuildctlPath string

    // Addr is passed to buildctl --addr when non-empty. Empty lets buildctl use
    // its own default BuildKit endpoint.
    Addr string
}
```

Notes:

- `Reference` should be canonicalized using the existing `CanonicalImageReference`.
- `Platform` should follow the existing defaulting rules: empty OS means `linux`, empty architecture means host `runtime.GOARCH`.
- `CreatedAt` and `UpdatedAt` are local store timestamps. They do not mean registry-created or registry-updated timestamps.
- `Digest` is the selected image target descriptor digest in the shared OCI layout.
- `SizeBytes` is the sum of unique reachable descriptor/blob sizes for the selected image target. It is not the total shared layout directory size and not the number of bytes newly written by a pull/build.
- `Layout(ctx)` returns the store-owned canonical OCI image layout path. It is the same path for every image in the store.
- `PullIfMissing` returns an existing matching `Image` record unchanged when present and valid.
- `PullAlways` refreshes the shared layout reference and metadata for the canonical reference/platform. Preserve `CreatedAt`; set `UpdatedAt` to the successful commit time.
- `Build` always builds and replaces the canonical reference/platform. Preserve `CreatedAt` when replacing an existing record; set `CreatedAt` and `UpdatedAt` to the commit time for a new record.
- `BuildRequest.ContextPath` must be an absolute existing directory.
- Empty `BuildRequest.DockerfilePath` means `<ContextPath>/Dockerfile`.
- Non-empty `DockerfilePath` must be an absolute existing file inside `ContextPath` for v1.
- Do not put `Delete()` on `Image`.

Add a build-specific error code to `pkg/shared/errors`:

```go
// ErrBuildFailed means image build work failed after request/config validation.
ErrBuildFailed Code = "build_failed"
```

Use existing codes where they fit: invalid build requests use `ErrInvalidRequest`, missing image records use `ErrImageNotFound`, corrupt layouts use `ErrInvalidImageLayout`, metadata backend failures use `ErrMetadataFailed`, and filesystem operations use `ErrFilesystemFailed`.

## Bundle Provisioning Handoff

The shared layout makes `ImageRef` alone too weak when a layout can contain more than one descriptor for the same reference across refreshes or platforms. Update `pkg/bundle.ProvisionRequest` so callers can pass the selected image identity returned by `image.Store`:

```go
type ProvisionRequest struct {
    ContainerID string
    ImageLayout string
    ImageRef    string
    ImageDigest string
    ImagePlatform image.Platform
    Process ProcessSpec
    Mounts  []Mount
}
```

Rules:

- `ImageLayout` is usually `layoutPath, err := imageStore.Layout(ctx)`.
- `ImageRef` remains required for compatibility with image-layout annotations and useful errors.
- `ImageDigest` should be supplied by daemon/CI callers from `image.Image.Digest`.
- `ImagePlatform` should be supplied from `image.Image.Platform`.
- The directory provisioner should prefer an exact digest/platform match when these fields are set. Falling back to `ImageRef` only is acceptable for old single-image layouts.
- The shared layout index should still maintain at most one descriptor for a canonical reference/platform pair.

## Factory Changes

Replace `factory.NewPuller` with `factory.NewStore`.

```go
func NewStore(config chamberImage.Config, directoryManager localfs.DirectoryManager) (chamberImage.Store, error)
```

The constructor should:

- reject a nil `directoryManager`;
- reject an empty `config.Root`;
- create `config.Root` with `directoryManager.MkdirPrivate`;
- create the canonical shared layout directory, metadata directory, and temp directory;
- initialize an empty OCI image layout when needed;
- create and return the concrete store implementation.

Do not require BuildKit configuration in `NewStore`. Pull/list/remove/layout should work without BuildKit. Validate `BuildKitConfig` only when `Store.Build` is called.

Remove public use of `factory.NewPuller` in daemon, CI, tests, and README examples.

## Internal Package Responsibilities

### `pkg/image/internal/store`

This is the concrete filesystem-backed image store.

It should own:

- configured root validation;
- private image root setup;
- canonical shared OCI layout setup;
- temporary operation directories;
- OCI layout validation before commit;
- the shared-layout commit protocol;
- safe reference replacement for `PullAlways` or build overwrite;
- coordination with `internal/metadata` for image record writes, reads, lists, and deletes;
- cleanup of temp and backup directories after failed operations.

Suggested store shape:

```text
<image-root>/
  layout/
    oci-layout
    index.json
    blobs/
      sha256/
        ...
  metadata/
    images/
      <image-key>.json
  tmp/
    ...
```

Empty layout behavior:

- `NewStore` must create `<image-root>/layout` as an OCI image layout with `oci-layout`, `index.json`, and `blobs/`.
- An empty store layout may have no manifests. That is valid as store infrastructure, but it is not a consumable image for provisioning.
- Do not loosen `ValidateLayoutContext` to accept empty layouts for normal image validation. Add an internal helper if the store needs to validate empty store infrastructure separately.
- `Layout(ctx)` may return the canonical layout path before any image exists.

Shared-layout commit protocol:

1. Fetch or build into a temp OCI image layout under `<image-root>/tmp`.
2. Validate the temp layout and resolve exactly one selected target descriptor for the canonical reference and platform.
3. Acquire the store's mutation lock before changing `<image-root>/layout`, `<image-root>/metadata`, or shared layout index state.
4. Re-check existing metadata under the lock. For `PullIfMissing`, return the existing valid record and discard the temp output if another operation already committed the image.
5. Copy missing blobs from the temp layout into `<image-root>/layout/blobs/<algorithm>/<digest>` using temp-then-rename per blob. Existing blobs with the same digest may be reused after size/digest validation.
6. Rewrite `<image-root>/layout/index.json` so there is at most one descriptor for the canonical reference and platform. The descriptor must carry `org.opencontainers.image.ref.name` for the canonical reference and platform metadata when available.
7. Write `index.json` with temp file, file sync, rename, and parent-directory sync.
8. Write the metadata record last through `internal/metadata.Metadata`.
9. Clean temp output after success or failure.

Commit and recovery semantics:

- Metadata is the authoritative source for `Store.List`, `Store.Remove`, and `PullIfMissing` cache hits.
- If the process fails after `index.json` is updated but before metadata is written, the image is not considered committed. A later pull/build can overwrite the orphaned index entry for the same reference/platform.
- If metadata exists but the matching descriptor/blob graph is missing or invalid, `PullIfMissing` must not treat it as a cache hit. Return a clear `ErrInvalidImageLayout` or refresh with `PullAlways`.
- The v1 store should serialize mutating operations on a single `Store` instance with a mutex. SDK callers still own coordination across multiple processes or separately constructed stores that share one root.

Important removal rules:

- `Remove` removes an image metadata record and its reference from the shared layout index.
- `Remove` must not accept arbitrary raw paths from callers.
- `Remove` must not delete the entire shared layout directory.
- `Remove` should not prune shared blobs in the first implementation. Blob pruning can come later as explicit store GC.
- `Remove` is idempotent for missing metadata records so interrupted removals and repeated cleanup calls are safe.
- `Remove` performs logical image cleanup only in v1. It removes metadata and index references but does not prove the image's blobs are unreferenced.
- `Layout(ctx)` must return only the configured shared layout path under `config.Root`.

Removal protocol:

1. Acquire the store mutation lock.
2. Delete the metadata record first so interrupted removal does not leave `List` reporting an image whose index entry has already been removed.
3. Rewrite `index.json` to remove matching descriptors for the requested canonical reference/platform.
4. Sync the rewritten index and parent directory.
5. Leave blobs in place.
6. Treat a missing metadata record as removable/idempotent, but still attempt to remove matching index descriptors based on the request.

### `pkg/image/internal/metadata`

This package should contain the store's durable local image metadata mechanics.

It should expose an internal metadata interface so the filesystem JSON backend can be swapped for SQLite later without changing `internal/store` orchestration:

```go
type Metadata interface {
    Put(ctx context.Context, img image.Image) error
    Get(ctx context.Context, reference string, platform image.Platform) (image.Image, error)
    List(ctx context.Context, request image.ListRequest) ([]image.Image, error)
    Delete(ctx context.Context, reference string, platform image.Platform) error
}
```

This interface is internal to `pkg/image`; it is not part of the public SDK. Keep it small and operation-shaped around what the image store actually needs.

It should own:

- metadata root setup;
- metadata key derivation from canonical reference and platform;
- JSON encoding and decoding for image records;
- atomic file writes using temp file, file sync, rename, and parent directory sync;
- read, list, and delete operations;
- validation of decoded records;
- clear errors for missing or corrupt metadata.

Suggested sidecar fields:

```go
type metadataFile struct {
    Reference string       `json:"reference"`
    Digest    string       `json:"digest"`
    Platform  image.Platform `json:"platform"`
    Source    image.Source `json:"source"`
    SizeBytes int64        `json:"size_bytes"`
    CreatedAt time.Time    `json:"created_at"`
    UpdatedAt time.Time    `json:"updated_at"`
}
```

Use names and formatting that fit the actual code. The above is a sketch, not a required exact struct.

Provide a concrete JSON implementation for v1:

```go
type JSONMetadata struct {
    root string
    directoryManager localfs.DirectoryManager
}

var _ Metadata = (*JSONMetadata)(nil)
```

Use names and constructor shapes that fit the actual implementation; `JSONMetadata` is a sketch.

The atomic JSON behavior should still be tested against the concrete JSON implementation on a real temporary filesystem. The interface supports backend substitution, but it does not prove that temp-file, sync, rename, and parent-directory sync behavior is correct.

### `pkg/image/internal/registry`

This package should contain registry-specific pull mechanics.

It should own:

- parsing the canonical reference into `go-containerregistry` types;
- auth option construction;
- remote image fetching;
- digest resolution;
- platform option construction;
- writing fetched images into a caller-provided temporary OCI layout location.

It should not own:

- image root creation;
- cache listing;
- removal policy;
- shared layout commits.

The store should call registry helpers from `Store.Pull`.

### `pkg/image/internal/buildkit`

This package should contain Dockerfile build mechanics.

Use BuildKit as the initial backing implementation through the `buildctl` CLI, not the BuildKit Go client. BuildKit is Dockerfile-native, cache-aware, supports rootless operation, and can export OCI outputs. Its docs describe Dockerfile frontends, OCI tarball output, image-store output, and cache behavior:

- https://github.com/moby/buildkit

First implementation should stay narrow:

- local build context only;
- local Dockerfile only;
- single platform only;
- no push;
- no secrets;
- no SSH forwarding;
- no remote Git contexts;
- no multi-platform manifest lists.

Concrete v1 behavior:

- `Store.Build` requires `image.Config.BuildKit.BuildctlPath`.
- If `BuildctlPath` is empty, `Build` returns `ErrInvalidRequest`.
- If `buildctl` cannot start, cannot reach BuildKit, or exits non-zero, `Build` returns `ErrBuildFailed`.
- `BuildKitConfig.Addr` is optional. When non-empty, pass `--addr <addr>` to `buildctl`.
- `buildkit` should run a command equivalent to:

  ```sh
  buildctl [--addr <addr>] build \
    --frontend dockerfile.v0 \
    --local context=<context-path> \
    --local dockerfile=<dockerfile-dir> \
    --opt filename=<dockerfile-base> \
    --output type=oci,dest=<tmp-output.tar>
  ```

- Add `--opt target=<target>` only when `BuildRequest.Target` is non-empty.
- Add build args as Dockerfile frontend options, for example `--opt build-arg:NAME=value`.
- Apply the requested single platform with `--opt platform=<os>/<arch>[/<variant>]`.
- Set `--local dockerfile=<dockerfile-dir>` and `--opt filename=<dockerfile-base>` from the resolved `DockerfilePath`.
- Import or unpack the OCI tar into a temp OCI image layout, validate it, and let `internal/store` commit it into the shared layout.
- Do not auto-install `buildctl` or `buildkitd`.

Do not require Docker Engine. Chamber should build through BuildKit, not the Docker daemon.

For unit tests, put command execution behind a small unexported runner dependency in `internal/buildkit` so tests can assert the command shape and simulate build failures without a real BuildKit daemon.

## Implementation Steps

1. Update public image contracts.

   - Remove `type Puller interface`.
   - Add `type Store interface`.
   - Add `BuildRequest`, `Image`, `Source`, `ListRequest`, and `RemoveRequest`.
   - Add `BuildKitConfig` to `image.Config`.
   - Add `ErrBuildFailed` to `pkg/shared/errors`.
   - Update the `ErrMetadataFailed` comment so it covers Chamber metadata storage generally, not only daemon metadata.
   - Keep `PullRequest`, `PullPolicy`, `Platform`, and `Auth`.
   - Remove or replace public uses of `PulledImage`.
   - Remove `LayoutPath` from public image result records.

2. Add `pkg/image/internal/metadata`.

   - Define a small internal `Metadata` interface for image metadata persistence.
   - Add a concrete JSON implementation that satisfies that interface.
   - Implement image metadata key derivation.
   - Implement atomic JSON writes with temp file, file sync, rename, and parent directory sync.
   - Implement read, list, and delete helpers.
   - Validate decoded metadata before returning public `image.Image` values.
   - Add focused tests for atomic writes, missing records, corrupt records, list filtering, and delete behavior.
   - If failure injection is needed for file sync, rename, or parent-directory sync cases, add a narrow unexported file-operations dependency inside `internal/metadata`.

3. Add `pkg/image/internal/store`.

   - Move or reimplement shared layout setup, temp, validation, commit, replacement, metadata, list, remove, and `Layout(ctx)` logic there.
   - Implement the shared-layout commit protocol described above.
   - Implement store-instance mutation locking for pull/build/remove commits.
   - Replace `DestinationForCanonicalImage` with helpers for the canonical shared layout path and image metadata keys.
   - Reuse existing helpers such as `ValidateLayoutContext` and size measurement logic where appropriate.
   - Replace per-image destination semantics with one canonical layout path, likely `<config.Root>/layout`.
   - Depend on the `internal/metadata.Metadata` interface for durable image record reads, writes, lists, and deletes.
   - Keep `localfs.DirectoryManager` values named `directoryManager`.

4. Refactor pull logic.

   - Move registry-specific code from `pkg/image/internal/puller` to `pkg/image/internal/registry`.
   - Implement `Store.Pull` in `internal/store`.
   - Delete `pkg/image/internal/puller` if it becomes empty.

5. Add BuildKit build logic.

   - Add `pkg/image/internal/buildkit`.
   - Use `buildctl` via `image.Config.BuildKit.BuildctlPath`.
   - Implement build request validation.
   - Produce a temp OCI tar or layout output.
   - Add a fakeable unexported command runner for tests.
   - Let `internal/store` validate and atomically commit the result.

6. Update bundle provisioning handoff.

   - Add `ImageDigest` and `ImagePlatform` to `bundle.ProvisionRequest`.
   - Update the directory provisioner to prefer exact digest/platform matching when supplied.
   - Keep `ImageRef` required and preserve the existing ref-only path for old single-image test layouts.

7. Update factory.

   - Delete `NewPuller`.
   - Add `NewStore`.
   - Ensure `daemon/config` still imports only root SDK contract packages and never imports `pkg/image/factory` or `pkg/image/internal`.

8. Update daemon and CI composition.

   - Replace fields, constructor params, fakes, and tests that currently mention `image.Puller`.
   - Use `image.Store` for image pull operations.
   - Remove daemon reliance on per-image `LayoutPath`.
   - When provisioning, call `layoutPath := imageStore.Layout(ctx)` and pass `layoutPath`, `image.Reference`, `image.Digest`, and `image.Platform` to the bundle provisioner.
   - Update daemon metadata image records to stop persisting SDK layout paths. Daemon metadata may keep daemon-owned timestamps such as `LastUsedAt`, but SDK image metadata owns `CreatedAt` and `UpdatedAt`.
   - Keep daemon image cleanup/list endpoints separate if they are not implemented yet; do not add broad daemon GC in this change.

9. Update docs.

   - Replace README references to `NewPuller` and `Puller` with `NewStore` and `Store`.
   - Clarify cleanup:
     - SDK callers decide when cleanup is safe.
     - `image.Store.Remove` owns safe logical removal from image metadata and shared layout index.
     - `image.Store.Layout(ctx)` returns the shared OCI image layout path.
     - `chamberd` will eventually own leases, references, and GC policy.

10. Delete stale names.

   Run searches and remove stale public references:

   ```sh
   rg -n "Puller|NewPuller|internal/puller|chamberImagePuller|pkg/image/puller"
   ```

## Test Plan

Use the normal Chamber Go cache:

```sh
env GOCACHE=/tmp/chamber-go-cache go test -run '^$' ./pkg/... ./daemon/... ./cmd/...
env GOCACHE=/tmp/chamber-go-cache go test ./pkg/... ./daemon/... ./cmd/...
```

Add or update tests for:

- `factory.NewStore` rejects nil directory managers.
- `factory.NewStore` rejects empty roots.
- `factory.NewStore` creates the private image root.
- `Store.Pull` preserves current pull behavior.
- `Store.Pull` reuses existing image records for `PullIfMissing`.
- `Store.Pull` atomically refreshes the image record and shared layout references for `PullAlways`.
- `Store.PullIfMissing` preserves `CreatedAt` and `UpdatedAt` for cache hits.
- `Store.PullAlways` and `Store.Build` preserve `CreatedAt` and advance `UpdatedAt` when replacing an existing reference/platform.
- `Store.Build` returns `ErrInvalidRequest` when `BuildctlPath` is empty.
- `Store.Build` validates context and Dockerfile paths according to the v1 path rules.
- `internal/buildkit` builds the expected `buildctl` command and maps command failures to `ErrBuildFailed` using a fake runner.
- Failed pull leaves no committed image record or partial shared-layout update.
- Failed build leaves no committed image record or partial shared-layout update.
- Concurrent pull/build/remove calls on one `Store` instance cannot corrupt `index.json` or metadata.
- `internal/metadata` writes records with temp-file, file sync, rename, and parent-directory sync, tested against the concrete implementation.
- `internal/metadata` preserves the previous valid record when an injected write/commit failure occurs before replacement.
- `internal/metadata` reports corrupt JSON records clearly and does not silently return partial image data.
- Successful pull/build commits a valid image entry into the shared OCI layout.
- `Image.SizeBytes` is computed as unique reachable target/config/layer bytes for the selected image, not shared layout directory size.
- `Store.List` returns committed image records and ignores temp/backup directories.
- `Store.Layout(ctx)` returns an initialized empty shared layout before any image is committed.
- `Store.Layout(ctx)` returns the canonical shared OCI layout path under `config.Root`.
- `Store.Remove` removes image records and shared-layout index references without deleting the whole layout.
- `Store.Remove` rejects or avoids arbitrary paths outside `config.Root`.
- `Store.Remove` leaves blobs in place in v1.
- `bundle.ProvisionRequest` with `ImageDigest`/`ImagePlatform` resolves the exact descriptor from a shared layout.
- Ref-only provisioning still works for old single-image layouts.
- Daemon composition depends on `image.Store`, not `image.Puller`.
- Daemon provisioning calls `imageStore.Layout(ctx)` instead of reading `image.LayoutPath`.
- Public `pkg/...` code does not import daemon packages.
- `daemon/config` does not import `pkg/image/factory` or `pkg/image/internal`.

Optional integration tests:

```sh
CHAMBER_INTEGRATION=1 env GOCACHE=/tmp/chamber-go-cache go test -count=1 ./pkg/image/... -run 'Test.*Real'
```

Gate real BuildKit tests behind an explicit environment variable because BuildKit daemon availability differs on macOS, Linux, and CI.

## Non-Goals

Do not include these in the first implementation:

- daemon-owned image GC;
- leases or reference counting;
- push support;
- image signing;
- image scanning;
- multi-platform build output;
- remote build contexts;
- Docker Engine dependency;
- Docker-compatible local image store integration;
- broad BuildKit cache pruning API.

## Acceptance Criteria

The implementation is done when:

- `image.Store` is the only public image-operation interface.
- `image.Puller` and `factory.NewPuller` are gone.
- Pulling commits valid image entries into the shared OCI layout under `image.Config.Root`.
- Dockerfile builds can commit valid image entries into the same shared OCI layout.
- `Image` has no layout path field; callers use `Store.Layout(ctx)` for the shared OCI layout path.
- Shared-layout commits and removals follow the documented lock, index, metadata, and fsync protocol.
- BuildKit v1 is implemented through the configured `buildctl` runner with fake-runner unit tests.
- Store removal is path-safe, rooted in `image.Config.Root`, removes metadata/index references, leaves blobs in place, and does not delete the shared layout.
- Bundle provisioning can select the intended descriptor from the shared layout using digest/platform.
- Existing daemon pull behavior works through `image.Store`.
- Existing daemon run/provision behavior obtains the layout path from `image.Store.Layout(ctx)` instead of persisted SDK image paths.
- Tests pass with the commands in the test plan, except for any known sandbox listener limitations that require an outside-sandbox rerun.
- Documentation reflects the new store-owned storage mechanics and caller-owned cleanup decision.
