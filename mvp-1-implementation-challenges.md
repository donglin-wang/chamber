# MVP Phase 1: Pull and Run

This phase turns the rootless `runc` proof of concept into a small daemon. The
goal is not to build a general container engine. The goal is to learn the path
from one HTTP request to durable metadata, an OCI image on disk, an OCI runtime
bundle, and a rootless runtime process.

The worksheet is intentionally written as a sequence of Go implementation
challenges. Each challenge introduces the interface or type it needs at the
point where you will implement or test it. Do not read the interfaces as a
separate architecture dump; treat each one as the smallest contract required by
the next exercise.

## What this phase proves

When the daemon starts, it:

1. resolves and validates user-private paths;
2. opens the metadata store;
3. makes sure the selected OCI runtime binary is available;
4. listens for HTTP over a user-private Unix socket;
5. accepts one pull endpoint and one run endpoint;
6. records operation, image, and container state durably;
7. correlates each request, store call, image action, and runtime action through
   durable correlation fields and structured logs, with config and context
   seams that can support OpenTelemetry later.

This phase deliberately excludes container-log retrieval, cancellation,
removal, garbage collection, networking, recovery of live processes, and Docker
API compatibility. Those belong to later reliability layers in `plan.md`.

## Docker-like, not Docker-compatible

Docker does not have a single daemon endpoint named "run". Docker clients create
a container and then start it with separate requests. Chamber uses one `run`
request in this phase because the MVP asks for only two endpoints.

| Operation | Method and path | Success |
| --- | --- | --- |
| Pull | `POST /v1/images/pull` | `200 OK` |
| Run | `POST /v1/containers/run` | `201 Created` |

The API borrows Docker's JSON-over-HTTP style and familiar status codes, but it
is a Chamber API. Do not claim Docker API compatibility. Compatibility would
force request fields, version negotiation, streaming behavior, and create/start
semantics before Chamber's own reliability contract is understood.

## The three kinds of state

Keep these separate throughout the implementation:

- Metadata: image records, operation records, container records, and state
  transitions. Phase 1 uses embedded etcd for this.
- Filesystem artifacts: OCI image layouts, unpacked root filesystems, runtime
  bundles, runtime binaries, socket files, logs, and temporary files.
- Diagnostic signals: trace-correlation fields, future metrics, and daemon logs.

etcd stores metadata only. It should contain paths, digests, timestamps, states,
and stable error codes. It should not contain image layers, runtime bundles, log
files, or diagnostic output.

Diagnostic signals are not the source of truth. A future collector can be
unavailable, a trace can be sampled away, and a metric point can be dropped.
Durable records must still explain what Chamber was doing after a restart.

## Work through the challenges in order

Commit after each challenge if the tests are green. The intended package layout
will emerge as you go:

```text
daemon/cmd/chamberd/main.go
daemon/api/http.go
daemon/config/config.go
daemon/service.go
internal/bundle/bundle.go
internal/bundle/umoci/provisioner.go
internal/image/puller.go
internal/metadata/store.go
internal/metadata/etcd/store.go
internal/runtime/runtime.go
internal/runtime/runc/runtime.go
internal/shared/testutil/memorystore.go
```

`daemon` will own the use-case ordering. HTTP handlers decode and
encode HTTP; they do not pull images, write etcd keys, provision bundles, or
invoke `runc` directly. Adapters under `internal/image`,
`internal/bundle/umoci`, `internal/metadata/etcd`, and `internal/runtime/runc`
deal with outside systems.

The package name `runtime` overlaps with Go's standard-library package. That is
legal, but import Chamber's package with an explicit alias such as `chruntime`
when both appear in one file.

---

## Challenge 1: encode container and operation state

The first package owns the durable vocabulary of the daemon: images,
containers, operations, and their allowed transitions. Start here because every
later package relies on these records.

Create `internal/metadata/store.go`:

```go
package metadata

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound      = errors.New("metadata: not found")
	ErrAlreadyExists = errors.New("metadata: already exists")
	ErrStateConflict = errors.New("metadata: state conflict")
)

type Image struct {
	// Reference is the user-facing name, for example
	// docker.io/library/alpine:latest.
	Reference string `json:"reference"`

	// Digest is the immutable manifest digest resolved by the puller.
	Digest string `json:"digest"`

	// LayoutPath is an absolute path to an OCI image-layout directory.
	LayoutPath string `json:"layout_path"`

	PulledAt time.Time `json:"pulled_at"`
}

type ContainerState string

const (
	ContainerCreating ContainerState = "creating"
	ContainerStarting ContainerState = "starting"
	ContainerRunning  ContainerState = "running"
	ContainerExited   ContainerState = "exited"
	ContainerFailed   ContainerState = "failed"
)

type Container struct {
	ID          string         `json:"id"`
	OperationID string         `json:"operation_id"`
	TraceID     string         `json:"trace_id,omitempty"`
	SpanID      string         `json:"span_id,omitempty"`
	ImageDigest string         `json:"image_digest"`
	ImageRef    string         `json:"image_ref"`
	BundlePath  string         `json:"bundle_path"`
	Runtime     string         `json:"runtime"`
	State       ContainerState `json:"state"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	ExitCode    *int           `json:"exit_code,omitempty"`
	ErrorCode   string         `json:"error_code,omitempty"`
}

type OperationKind string

const (
	OperationPull OperationKind = "pull"
	OperationRun  OperationKind = "run"
)

type OperationState string

const (
	OperationRunning   OperationState = "running"
	OperationSucceeded OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
)

type Operation struct {
	ID         string         `json:"id"`
	Kind       OperationKind  `json:"kind"`
	State      OperationState `json:"state"`
	ResourceID string         `json:"resource_id"`
	TraceID    string         `json:"trace_id,omitempty"`
	SpanID     string         `json:"span_id,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`

	// ErrorCode is a stable machine-readable category such as
	// "image_not_found" or "runtime_start_failed". Do not persist registry
	// credentials, command arguments, or an unfiltered external error.
	ErrorCode string `json:"error_code,omitempty"`
}

func ValidTransition(from, to ContainerState) bool {
	// TODO: implement the transition table below.
	panic("TODO")
}

func ValidOperationTransition(from, to OperationState) bool {
	// TODO: implement the operation transition table below.
	panic("TODO")
}
```

Allow only these container transitions:

```text
creating -> starting
creating -> failed
starting -> running
starting -> failed
starting -> exited
running  -> exited
running  -> failed
```

Allow only these operation transitions:

```text
running -> succeeded
running -> failed
```

Use this closed set of durable error codes in Phase 1:

```text
invalid_request
image_not_found
pull_failed
metadata_failed
bundle_prepare_failed
runtime_start_failed
runtime_wait_failed
container_exit_nonzero
state_conflict
```

These durable codes are more specific than the public HTTP error set. For
example, `bundle_prepare_failed` and `runtime_start_failed` both map to the
public `internal` code.

Write table-driven tests for every allowed transition and several forbidden
ones. Include same-state transitions as forbidden.

You are practicing:

- named string types and constants;
- table-driven tests;
- distinguishing domain rules from storage mechanics.

Done when:

```sh
go test ./internal/metadata
```

passes.

---

## Challenge 2: define the metadata store boundary

Now that the records exist, define how the rest of Chamber is allowed to change
them. The important design pressure is concurrency: two goroutines must not be
able to both read `starting`, choose different terminal states, and overwrite
each other.

Extend `internal/metadata/store.go`:

```go
type Store interface {
	PutImage(ctx context.Context, image Image) error
	GetImage(ctx context.Context, reference string) (Image, error)

	CreateOperation(ctx context.Context, operation Operation) error
	GetOperation(ctx context.Context, id string) (Operation, error)
	TransitionOperation(
		ctx context.Context,
		id string,
		from OperationState,
		update OperationUpdate,
	) (Operation, error)

	CreateContainer(ctx context.Context, container Container) error
	GetContainer(ctx context.Context, id string) (Container, error)
	TransitionContainer(
		ctx context.Context,
		id string,
		from ContainerState,
		update ContainerUpdate,
	) (Container, error)

	Close() error
}

type ContainerUpdate struct {
	State     ContainerState
	At        time.Time
	ExitCode  *int
	ErrorCode string
}

type OperationUpdate struct {
	State     OperationState
	At        time.Time
	ErrorCode string
}
```

`TransitionContainer` is a compare-and-swap operation. It succeeds only when the
current state equals `from`. The implementation also sets `UpdatedAt`,
`ExitCode`, and `ErrorCode` from the update. `TransitionOperation` sets both
`UpdatedAt` and `FinishedAt` from `At` so callers cannot provide contradictory
terminal timestamps.

Write interface-level tests as a contract that can later run against memory and
etcd implementations:

```go
func TestStoreContract(t *testing.T) {
	tests := map[string]func(t *testing.T) metadata.Store{
		// Add implementations as you build them.
	}
	for name, newStore := range tests {
		t.Run(name, func(t *testing.T) {
			store := newStore(t)
			t.Cleanup(func() { _ = store.Close() })
			// TODO: shared behavior assertions.
		})
	}
}
```

For now, write the assertions even if the implementation map is empty. The
contract should cover:

- image put/get;
- missing image returning `ErrNotFound`;
- duplicate operation or container returning `ErrAlreadyExists`;
- valid state transitions;
- invalid state transitions returning `ErrStateConflict`;
- operation terminal timestamps coming from `OperationUpdate.At`.

You are practicing:

- interface design from caller needs;
- sentinel errors;
- compare-and-swap semantics;
- writing reusable contract tests before adapters exist.

---

## Challenge 3: resolve every path before touching disk

Chamber must be per-user and rootless by default, so path calculation is a real
part of the correctness story. Do this before implementing side-effectful
adapters.

Create `daemon/config/config.go`:

```go
package config

import "time"

type Config struct {
	SocketPath    string
	TmpRoot       string
	ImageRoot     string
	ContainerRoot string
	RuntimeRoot   string
	RuntimeBinDir string
	MetadataRoot  string

	RuntimeName    string
	RuntimeVersion string
	RuntimeURL     string
	RuntimeSHA256  string

	OpenTelemetryEndpoint              string
	OpenTelemetryInsecure              bool
	OpenTelemetryTraceSampleRatio      float64
	OpenTelemetryMetricsExportInterval time.Duration
	LogLevel              string
	LogFormat             string
}

type Override struct {
	SocketPath    *string
	TmpRoot       *string
	ImageRoot     *string
	ContainerRoot *string
	RuntimeRoot   *string
	RuntimeBinDir *string
	MetadataRoot  *string

	RuntimeName    *string
	RuntimeVersion *string
	RuntimeURL     *string
	RuntimeSHA256  *string

	OpenTelemetryEndpoint              *string
	OpenTelemetryInsecure              *bool
	OpenTelemetryTraceSampleRatio      *float64
	OpenTelemetryMetricsExportInterval *time.Duration
	LogLevel                           *string
	LogFormat                          *string
}

func Load(override Override, getenv func(string) string) (Config, error) {
	// TODO: derive defaults from getenv, then call Resolve.
	panic("TODO")
}

func Resolve(defaultConfig Config, override Override) (Config, error) {
	// TODO: apply typed overrides, convert every path to absolute, validate
	// values, and return the result without touching disk.
	panic("TODO")
}

func (c Config) Prepare() error {
	// TODO: create each directory as 0700 and reject directories not owned by
	// the effective UID or writable by group/other.
	panic("TODO")
}
```

Use these defaults:

| Field | Default |
| --- | --- |
| Root path used for defaults | `$XDG_DATA_HOME/chamber`, else `$HOME/.local/share/chamber` |
| `ImageRoot` | `<root>/images` |
| `ContainerRoot` | `<root>/containers` |
| `RuntimeBinDir` | `<root>/bin` |
| `MetadataRoot` | `<root>/metadata/etcd` |
| `SocketPath` | `<root>/run/chamber.sock` |
| `TmpRoot` | `<root>/run/tmp` |
| `RuntimeRoot` | `<root>/run/runtime` |
| `OpenTelemetryEndpoint` | empty; future observability setup should treat telemetry as disabled |
| `OpenTelemetryTraceSampleRatio` | `1.0` for the learning MVP |
| `OpenTelemetryMetricsExportInterval` | `10s` |
| `LogLevel` | `info` |
| `LogFormat` | `json` |

`Override` is the typed boundary between parsing and resolution. A nil pointer
means no user override. A non-nil pointer means the user explicitly supplied the
value, even when that value is an empty string, `false`, or `0`.

Do not parse CLI flags, JSON, YAML, or any other external config format inside
`Load`. Parsing, unknown-key rejection, duplicate-key handling, and
format-specific type errors belong near the entrypoint or parser for that
format. Those parsers should produce `Override`; this package should only turn
defaults plus `Override` plus `getenv` into a fully resolved `Config`.

The daemon entrypoint should eventually accept at least these override keys or
flags and translate them into `Override` fields:

```text
--tmp-root
--image-root
--container-root
--runtime-root
--runtime-bin-dir
--metadata-root
--socket
--runtime-name
--runtime-version
--runtime-url
--runtime-sha256
--otel-endpoint
--otel-insecure
--trace-sample-ratio
--metrics-export-interval
--log-level
--log-format
```

Do not use `os.TempDir()` for pull or bundle work. Every temporary artifact
must be created below `TmpRoot`, an image-root staging directory, or a
container-root staging directory.

Test:

- XDG variables present;
- XDG variables absent;
- explicit path overrides;
- relative paths becoming absolute;
- unsafe permissions being rejected;
- invalid sample ratios and export intervals;
- supported log levels and formats;
- telemetry configuration values can be represented without initializing any
  telemetry provider.

Use `t.TempDir()` for fake home and data directories in tests. Do not inspect
the real user's home directory.

You are practicing:

- representing partial user input separately from resolved config;
- dependency injection through `getenv`;
- filesystem modes and ownership;
- pure calculation before side effects.

---

## Challenge 4: build the first metadata implementation in memory

Before etcd enters the picture, build a small in-memory store. This gives you a
fast target for service tests and lets the store contract prove the behavior
without network, disk, or embedded-server complexity.

Create `internal/shared/testutil/memorystore.go`. Implement `metadata.Store` with a
`sync.RWMutex` and maps. Copy records on read and write. Enforce the same state
transition rules and sentinel errors that etcd will enforce later.

Add the memory store to the contract from Challenge 2:

```go
func TestStoreContract(t *testing.T) {
	tests := map[string]func(t *testing.T) metadata.Store{
		"memory": testMemoryStore,
		// Add "etcd" in Challenge 6.
	}
	// TODO: run the same behavior assertions for each implementation.
}
```

Run the tests with the race detector:

```sh
go test -race ./internal/metadata ./internal/shared/testutil
```

You are practicing:

- satisfying an interface implicitly;
- pointer aliasing and defensive copies;
- mutex selection;
- compare-and-swap semantics in memory.

---

## Challenge 5: make etcd one metadata implementation detail

Now implement the real MVP metadata backend. The rest of Chamber should still
know only about `metadata.Store`; etcd revisions, clients, transactions, and
key strings stay inside the adapter.

Create `internal/metadata/etcd/store.go`:

```go
package etcd

import (
	"context"
)

type Config struct {
	DataDir      string
	ClientSocket string
	PeerSocket   string
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	// TODO: configure embedded etcd with DataDir and unix:// listener URLs,
	// start it, wait for readiness with ctx, and return an implementation of
	// metadata.Store.
	panic("TODO")
}
```

Use embedded etcd so `--metadata-root` has an unambiguous meaning and the daemon
remains a single user-started process. This is heavier than an embedded
key/value library, but acceptable for this requested MVP adapter.

Default the client and single-member peer sockets below `MetadataRoot`, make
their parent directory `0700`, and use `unix://` listener URLs. A fixed
localhost TCP port would collide between users and would not use filesystem
permissions as the user boundary.

Use private keys:

```text
/chamber/v0/images/by-reference/<escaped-reference>
/chamber/v0/operations/<operation-id>
/chamber/v0/containers/<container-id>
```

Do not place raw image references directly into key paths. Encode references
with `base64.RawURLEncoding`.

Store each value as versioned JSON:

```go
type envelope[T any] struct {
	SchemaVersion int `json:"schema_version"`
	Value         T   `json:"value"`
}
```

Implement `TransitionContainer` with an etcd transaction:

1. read the current value and its modification revision;
2. decode and validate its state;
3. construct the updated value;
4. compare the key's modification revision with the one read;
5. put the new value only if the comparison succeeds;
6. return `ErrStateConflict` when the comparison fails.

The compare-and-swap guards concurrent writers. `ValidTransition` guards
invalid state-machine edges. Both checks are required.

Add `"etcd"` to the store contract test. Extend the contract to prove:

- an operation cannot skip directly between terminal states;
- compare-and-swap rejects two concurrent terminal updates;
- trace data is persisted only in explicit `TraceID` and `SpanID` fields.

You are practicing:

- adapting a concrete database to a small Go interface;
- JSON schema versioning;
- context deadlines;
- optimistic concurrency with etcd transactions;
- cleanup in tests with `t.Cleanup`.

---

## Challenge 6: pull an image atomically

Pulling an image is not an OCI runtime responsibility. An OCI runtime consumes a
bundle containing `config.json` and `rootfs/`; it does not speak to image
registries. This package writes a complete OCI image layout and reports the
immutable digest that later container records will use.

Create `internal/image/puller.go`:

```go
package image

import (
	"context"
	"time"
)

type PullRequest struct {
	Reference   string
	Destination string
	Platform    string
}

type PulledImage struct {
	Reference  string
	Digest     string
	LayoutPath string
	SizeBytes  int64
	PulledAt   time.Time
}

type Puller interface {
	// Pull writes a complete OCI image layout below Destination. It must write
	// to a temporary sibling first and rename only after verification succeeds.
	Pull(ctx context.Context, request PullRequest) (PulledImage, error)
}
```

Use `go-containerregistry` for the first implementation. The useful sequence
from the proof of concept was:

1. parse the image reference;
2. choose `linux/<GOARCH>`;
3. fetch with the default registry keychain;
4. write an OCI image layout;
5. obtain and record the resolved manifest digest.

The temporary-directory-then-rename rule prevents a failed pull from leaving a
directory that looks complete.

Test without depending on Docker Hub. Use a tiny local HTTP registry or inject a
fake lower-level fetch function. Cover:

- invalid image reference;
- unsupported platform;
- registry failure leaves no final OCI layout;
- rename failure is returned;
- a successful pull returns a digest, byte count, and UTC pull time.

Keep the `context.Context` parameter on the boundary even before telemetry is
implemented. It will eventually carry cancellation, deadlines, and trace
context into registry calls. Do not log registry credentials or
credential-bearing URLs.

You are practicing:

- contexts around network calls;
- temporary directories and atomic rename;
- wrapping errors with `%w`;
- separating registry data from daemon metadata.

---

## Challenge 7: define the OCI runtime boundary and verify `runc`

The runtime adapter has exactly two responsibilities in Phase 1: ensure the
configured runtime binary exists and start a container from an already
provisioned runtime bundle. Bundle provisioning is independent from runtime
execution because it translates an OCI image layout into the files and optional
mount instructions that the runtime will consume.

Create `internal/bundle/bundle.go` first. This package is deliberately neutral:
provisioners produce these values, and runtimes consume these values, but this
package does not know about either side.

If you are starting from the current repo state after Challenge 6,
`internal/bundle` does not exist yet, while `internal/runtime/runtime.go` already
exists with the older `PrepareRequest` and `BundlePreparer` types. In this
challenge, create `internal/bundle/bundle.go`, then update
`internal/runtime/runtime.go` in place: remove those older preparation types,
import `internal/bundle`, and make `RunRequest` carry the neutral bundle result.

```go
package bundle

import "context"

// Mount describes one filesystem mount that must exist before the runtime
// starts the container. Target is relative to the bundle's rootfs directory;
// an empty target means the rootfs directory itself.
type Mount struct {
	Type    string
	Source  string
	Target  string
	Options []string
}

type RootFS struct {
	// Mounts is empty when BundlePath/rootfs is already a populated directory.
	// Future overlayfs or snapshot-based provisioners can return mounts here
	// and leave the runtime responsible for applying and later unmounting them.
	Mounts []Mount
}

type ProvisionedBundle struct {
	ContainerID string
	BundlePath  string
	RootFS      RootFS
}

type ProvisionRequest struct {
	ContainerID string
	ImageLayout string
	ImageRef    string
	Command     []string
}

type Provisioner interface {
	// Provision creates the OCI runtime bundle for one container. It owns image
	// unpacking, spec generation or patching, temporary staging, and the atomic
	// move into the final bundle directory.
	Provision(ctx context.Context, request ProvisionRequest) (ProvisionedBundle, error)
}
```

For the first implementation, `ProvisionedBundle.RootFS.Mounts` will be empty
because `umoci` writes the final `rootfs/` directory directly. Keeping the field
now gives Chamber a containerd-shaped handoff for later overlayfs support:
provisioning can return mount instructions without making the runtime import the
provisioner or snapshot implementation.

Create `internal/runtime/runtime.go`:

```go
package runtime

import (
	"context"
	"io"

	chbundle "github.com/donglin-wang/chamber/internal/bundle"
)

type Binary struct {
	Name    string
	Version string
	Path    string
}

type RunRequest struct {
	Bundle    chbundle.ProvisionedBundle
	StateRoot string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

type Process interface {
	Wait() (exitCode int, err error)
}

type ObservedState string

const (
	ProcessRunning ObservedState = "running"
	ProcessExited  ObservedState = "exited"
)

type StartResult struct {
	Process Process
	State   ObservedState
}

type Runtime interface {
	// Ensure downloads the configured binary from a trusted HTTPS source when
	// absent, verifies its SHA-256 checksum, and returns its absolute path.
	Ensure(ctx context.Context) (Binary, error)

	// Run starts the OCI runtime process and returns only after the child has
	// either reached "running" or exited before that state could be observed.
	// Wait observes or returns its cached exit result.
	Run(ctx context.Context, binary Binary, request RunRequest) (StartResult, error)
}
```

`Runtime` depends on `bundle.ProvisionedBundle`, not on
`bundle.Provisioner`. The daemon service coordinates the two independent
interfaces.

Implement `Runtime.Ensure` in the `runc` adapter. Pin both a version and an
expected SHA-256 checksum. Download to a temporary file under `RuntimeBinDir`,
stream bytes through `sha256.New`, call `Sync`, set mode `0755`, and rename
into place only after the digest matches.

In the current repo state, `internal/runtime/runc/runtime.go` already contains
an `Ensure` implementation and `internal/runtime/runc/runtime_test.go` already
covers the download, checksum, cache-hit, and replacement cases below. Preserve
that implementation unless it needs small compile fixes after the boundary
change; the new work in this challenge is mostly the `internal/bundle` contract
and removing the old preparation interface from `internal/runtime`.

A URL alone is not a trust decision. HTTPS protects transport; the pinned digest
authenticates the expected artifact. Do not download a checksum and the binary
from the same untrusted location and treat that alone as independent
verification.

Test with `httptest.Server`:

- valid content and digest;
- wrong digest;
- non-200 response;
- interrupted body;
- existing valid binary;
- existing invalid binary being replaced.

Keep cache-hit versus download decisions visible through return values, errors,
or structured logs later, but do not add tracing or metrics in this challenge.

You are practicing:

- interface boundaries around process runtimes;
- streaming I/O;
- hashing;
- HTTP status handling;
- file modes;
- crash-safe replacement.

---

## Challenge 8: provision a rootless runtime bundle

The pulled OCI image layout is not yet something `runc` can execute:

```text
OCI image layout                 OCI runtime bundle
index.json                       config.json
oci-layout          ---->        rootfs/
blobs/sha256/...
```

Create the concrete bundle provisioner in
`internal/bundle/umoci/provisioner.go`:

```go
package umoci

import (
	"context"

	chbundle "github.com/donglin-wang/chamber/internal/bundle"
)

type Provisioner struct {
	ContainerRoot string
	UID            uint32
	GID            uint32
}

func (p Provisioner) Provision(
	ctx context.Context,
	request chbundle.ProvisionRequest,
) (chbundle.ProvisionedBundle, error) {
	// TODO:
	// 1. reject an unsafe container ID;
	// 2. create a temporary directory below ContainerRoot;
	// 3. unpack the selected image with umoci's Go API;
	// 4. decode config.json into specs.Spec;
	// 5. apply the rootless changes listed below;
	// 6. write config.json with mode 0600;
	// 7. atomically rename the temporary bundle into its final path;
	// 8. return chbundle.ProvisionedBundle with BundlePath set and
	//    RootFS.Mounts empty.
	panic("TODO")
}
```

Carry forward these concrete lessons from the proof of concept:

- map container UID 0 to the daemon user's effective UID;
- map container GID 0 to the daemon user's effective GID;
- use a user namespace;
- omit network and cgroup namespaces for the first narrow rootless path;
- clear `Linux.CgroupsPath` and `Linux.Resources`;
- remove cgroup mounts;
- replace `Process.Args` only when the request supplies a command.

This means the first container shares the host network namespace and has no
cgroup limits. That is a learning-stage limitation, not the finished rootless
isolation model described in `plan.md`.

Test spec patching as a pure function:

```go
func patchRootlessSpec(
	spec *specs.Spec,
	uid uint32,
	gid uint32,
	command []string,
) error {
	// TODO
	return nil
}
```

Assertions should inspect the decoded struct, not compare pretty-printed JSON.

For the first path, `RootFS.Mounts` stays empty because the provisioner writes a
real `rootfs/` directory. A later overlayfs provisioner can instead write
`config.json`, create an empty `rootfs/` mount point, and return one or more
mount descriptions for the runtime to apply before invoking `runc`.

Keep the `context.Context` parameter on provisioning so later instrumentation can
correlate unpack and spec-patching work. Do not log or emit the whole
`config.json`; it may contain environment variables or command arguments.

You are practicing:

- translating between OCI image and runtime formats;
- manipulating a structured specification safely;
- user-namespace ID mappings;
- cleaning partial filesystem work.

---

## Challenge 9: start and observe the runtime process

Now implement `Runtime.Run` for a process-based OCI runtime. The subtle part is
that `exec.Cmd.Start` proves the host launched the `runc` process, but it does
not prove that `runc` created the container's init process.

Invoke:

```text
runc --root <StateRoot> run <container-id>
```

Use `request.Bundle.ContainerID` as the runtime container ID and set `cmd.Dir` to
`request.Bundle.BundlePath`. If `request.Bundle.RootFS.Mounts` is non-empty,
create `<BundlePath>/rootfs`, apply those mounts before invoking `runc`, and
make the returned process handle responsible for unmounting after the container
exits. If startup fails before a process handle is returned, clean up any mounts
before returning the error.

Connect the provided standard streams. After `cmd.Start`, poll:

```text
runc --root <StateRoot> state <container-id>
```

Return only when one of these happens:

1. `state` reports OCI status `running`;
2. the runtime process exits before `running` is observed;
3. a short startup deadline expires.

Start exactly one goroutine that calls `cmd.Wait` and stores its result in the
process handle. The readiness loop and later `Process.Wait` must observe that
shared result rather than calling `cmd.Wait` twice. Convert `*exec.ExitError`
into a real exit code instead of erasing it behind a generic error.

Very short jobs may exit before the readiness poll sees `running`. Return
`ProcessExited` with a handle whose `Wait` returns the cached result. This lets
the service durably record `starting -> exited` instead of pretending the
container is still running.

Validate container IDs before passing them as arguments, even though
`exec.CommandContext` does not invoke a shell. Use a conservative rule such as:

```text
^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$
```

Create `oci.runtime.start` around launch and readiness polling. Add events for
`process.started`, `container.running`, and
`process.exited_before_observation`. Do not emit one event per readiness poll.

You are practicing:

- child process lifecycle;
- the difference between `Start` and `Run`;
- exit status extraction;
- why avoiding a shell prevents command injection but not all bad input.

---

## Challenge 10: orchestrate pull and run in the daemon service

This is where the earlier interfaces become useful. The service owns ordering:
durable record first, external side effect second, terminal transition last.

Create `daemon/service.go`:

```go
package daemon

import (
	"context"
	"log/slog"
	"time"

	chbundle "github.com/donglin-wang/chamber/internal/bundle"
	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/donglin-wang/chamber/internal/metadata"
	chruntime "github.com/donglin-wang/chamber/internal/runtime"
)

type Clock interface {
	Now() time.Time
}

type IDGenerator interface {
	New() string
}

type Correlator interface {
	TraceIDs(ctx context.Context) (traceID string, spanID string)
}

type Service struct {
	Store       metadata.Store
	Puller      chimage.Puller
	Runtime     chruntime.Runtime
	Binary      chruntime.Binary
	Provisioner chbundle.Provisioner
	Clock       Clock
	IDs         IDGenerator
	Lifetime    context.Context
	Correlator  Correlator
	Logger      *slog.Logger
	ImageRoot   string
	RuntimeRoot string
	Platform    string
}

type PullRequest struct {
	Reference string
}

type RunRequest struct {
	Image   string
	Command []string
}

type PullResult struct {
	Operation metadata.Operation
	Image     metadata.Image
}

type RunResult struct {
	Operation metadata.Operation
	Container metadata.Container
}

type Error struct {
	OperationID string
	Code        string
	Err         error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func (s *Service) Pull(
	ctx context.Context,
	request PullRequest,
) (PullResult, error) {
	// TODO: create the operation, validate, pull, persist image metadata,
	// transition the operation, and write correlated logs.
	panic("TODO")
}

func (s *Service) Run(
	ctx context.Context,
	request RunRequest,
) (RunResult, error) {
	// TODO: implement the ordered state transitions described below.
	panic("TODO")
}
```

`Pull` performs this exact sequence:

1. read trace and span IDs from `Correlator` when it is present;
2. generate an operation ID and create a `running` pull operation containing
   the correlation IDs, if any;
3. log `pull started` with the operation ID;
4. validate and pull into a staging path;
5. persist the resolved image metadata;
6. transition the operation to `succeeded`;
7. log completion without credentials or raw external errors;
8. on any error after operation creation, transition the operation to `failed`
   with a stable error code before returning.

`Run` performs this exact sequence:

1. read trace and span IDs from `Correlator` when it is present;
2. generate operation and container IDs;
3. create a `running` operation containing the correlation IDs, if any;
4. validate the image reference and command;
5. load the pulled image or fail the operation with `image_not_found`;
6. create a durable container record in `creating`, copying operation and trace
   correlation fields;
7. provision the runtime bundle, store the returned bundle path on the container
   record, and keep the full `ProvisionedBundle` for runtime start;
8. transition `creating -> starting`;
9. open stdout and stderr files below the container directory;
10. call `Runtime.Run` with the daemon-lifetime context and the provisioned
    bundle;
11. on a start error, fail both the container and operation records;
12. if the observed state is `running`, transition `starting -> running`;
13. if the process already exited, collect its cached result and transition
    `starting -> exited`;
14. return the operation and latest container records to the HTTP handler;
15. for a running process, call `Process.Wait` in a goroutine;
16. transition `running -> exited`, record the exit code, and
    finish the operation as `succeeded` for zero or `failed` for non-zero or
    runtime failure.

If provisioning fails, transition `creating -> failed`; never silently delete the
durable record or leave a final-looking bundle directory behind.

Use `Service.Lifetime`, not the HTTP request context, when calling
`Runtime.Run`. The server cancels the request context as soon as the response
finishes, while the child process belongs to the daemon. Preserve trace
correlation by copying the request's correlation IDs into the durable operation
and container records.

Return `*daemon.Error` after an operation exists. Its stable `Code` drives the
HTTP mapping and its `OperationID` lets the handler correlate failed responses.
Wrap the original error for server-side logging, but never send `Err.Error()`
directly to the client.

If recording a terminal operation or container state also fails, preserve both
errors with `errors.Join` and emit one correlated daemon log. Do not pretend the
terminal record is durable.

For this phase, write stdout and stderr to files inside the container
directory:

```text
<ContainerRoot>/<container-id>/stdout.log
<ContainerRoot>/<container-id>/stderr.log
```

Test against fakes first. Assert call order and durable states for:

- successful pull;
- pull succeeds on disk but metadata write fails;
- missing image on run;
- bundle provisioning fails;
- runtime start fails;
- runtime starts and exits zero;
- runtime starts and exits non-zero;
- two concurrent terminal-state updates, where only one wins.

For each case, also assert:

- an operation record exists before the first fake side effect;
- the operation reaches the correct terminal state and stable error code;
- the operation ID appears in the container record and correlated log;
- request contexts reach the puller, provisioner, runtime, and store fakes;
- the runtime fake receives the exact `ProvisionedBundle` returned by the
  provisioner, including any rootfs mounts;
- fake correlation IDs, when supplied, are copied to operation and container
  records;
- command arguments and injected fake credentials appear in no log.

Use channels in fakes to signal that `Wait` was called and to release it from
the test. Sleeps make concurrency tests slow and nondeterministic.

You are practicing:

- programming against interfaces;
- arranging durable state before and after side effects;
- goroutine ownership;
- testing asynchronous completion without `time.Sleep`.

---

## Challenge 11: expose the two HTTP endpoints

HTTP is a transport adapter. It should decode, validate, call the service,
choose a public status code, and encode a response. It should not know how to
pull an image, write etcd, provision bundles, or execute `runc`.

Create `daemon/api/http.go`:

```go
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/donglin-wang/chamber/daemon"
	"github.com/donglin-wang/chamber/internal/metadata"
)

type PullRequest struct {
	Reference string `json:"reference"`
}

type PullResponse struct {
	OperationID string    `json:"operation_id"`
	Reference   string    `json:"reference"`
	Digest      string    `json:"digest"`
	PulledAt    time.Time `json:"pulled_at"`
}

type RunRequest struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
}

type RunResponse struct {
	OperationID string                  `json:"operation_id"`
	ID          string                  `json:"id"`
	ImageDigest string                  `json:"image_digest"`
	State       metadata.ContainerState `json:"state"`
}

type ErrorResponse struct {
	OperationID string `json:"operation_id,omitempty"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

type Service interface {
	Pull(ctx context.Context, request daemon.PullRequest) (daemon.PullResult, error)
	Run(ctx context.Context, request daemon.RunRequest) (daemon.RunResult, error)
}

func NewHandler(service Service) http.Handler {
	mux := http.NewServeMux()
	// TODO: register exact method-and-path patterns, available in modern Go:
	// mux.HandleFunc("POST /v1/images/pull", ...)
	// mux.HandleFunc("POST /v1/containers/run", ...)
	return mux
}
```

Request bodies:

```json
{"reference":"docker.io/library/alpine:latest"}
```

```json
{
  "image":"docker.io/library/alpine:latest",
  "command":["/bin/sh","-c","echo hello"]
}
```

Successful responses:

```json
{
  "operation_id":"01J...PULL",
  "reference":"docker.io/library/alpine:latest",
  "digest":"sha256:...",
  "pulled_at":"2026-07-08T12:00:00Z"
}
```

```json
{
  "operation_id":"01J...RUN",
  "id":"01J...",
  "image_digest":"sha256:...",
  "state":"running"
}
```

Use this status mapping:

| Condition | Status | Error code |
| --- | --- | --- |
| malformed JSON, unknown field, empty required field | `400` | `invalid_request` |
| run refers to an image that has not been pulled | `404` | `image_not_found` |
| duplicate container ID or state compare-and-swap failure | `409` | `conflict` |
| pull succeeds | `200` | — |
| container starts | `201` | — |
| unexpected pull, store, bundle, or runtime failure | `500` | `internal` |

Decode exactly one JSON value, reject unknown fields with
`Decoder.DisallowUnknownFields`, and cap request bodies with
`http.MaxBytesReader`. Use `Content-Type: application/json` for every response.
Do not return raw internal errors to clients; log the wrapped error server-side
and return a stable public message.

Set `X-Chamber-Operation-ID` on every response for which an operation was
created, including failures. Do not set it for malformed requests rejected
before service entry. If a future trace propagation layer reads headers such as
`traceparent`, keep that separate from the operation ID; the operation ID is a
durable Chamber identifier, not a tracing protocol.

Test with `httptest.NewRecorder` and a fake service:

- wrong method;
- wrong path;
- body larger than the limit;
- malformed JSON;
- second JSON value;
- unknown field;
- every error-to-status mapping;
- exact response `Content-Type`;
- success response bodies;
- `X-Chamber-Operation-ID` on successes and post-creation failures;
- no operation header for malformed requests rejected before service entry;
- request contexts are passed through to the service.

You are practicing:

- the narrow responsibility of a transport adapter;
- strict JSON decoding;
- stable public errors;
- dependency inversion in ordinary Go.

---

## Challenge 12: compose and stop the daemon

Write `daemon/cmd/chamberd/main.go` last. It is the composition root and may
import every concrete adapter. Inner packages should depend only on interfaces.

Keep `main` tiny:

```go
func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		slog.Error("chamberd stopped", "error", err)
		os.Exit(1)
	}
}
```

Build startup in this order:

```text
parse configuration
  -> prepare and verify directories
  -> initialize slog
  -> reject non-Linux or effective UID 0
  -> check rootless user-namespace prerequisites
  -> open embedded etcd
  -> ensure and verify the runtime binary
  -> construct puller, provisioner, service, and HTTP handler
  -> remove only a stale socket owned by this user
  -> net.Listen("unix", SocketPath)
  -> chmod socket 0600
  -> http.Server.Serve(listener)
```

Generate a fresh random service instance ID for each daemon process and include
it in startup logs. Keep the value available to a future observability adapter,
but do not initialize OpenTelemetry in this phase. Do not log the username,
home directory, command arguments, or configured storage paths.

Do not remove an existing socket merely because `net.Listen` reports that the
path exists. First attempt to connect. If a server answers, another daemon owns
the path and startup must fail. Only remove it when it is a socket, no server is
listening, and it is owned by the current effective UID.

Handle `SIGINT` and `SIGTERM`. On shutdown:

1. stop accepting requests with `http.Server.Shutdown`;
2. wait for in-flight handlers up to a configured timeout;
3. close the metadata store;
4. close the Unix listener;
5. remove the socket path.

Verify socket mode, signal shutdown, stale-socket behavior, failure cleanup,
logger initialization, and handler construction.

HTTP over a Unix socket can be tested with:

```sh
curl --unix-socket "$HOME/.local/share/chamber/run/chamber.sock" \
  -H 'Content-Type: application/json' \
  -d '{"reference":"docker.io/library/alpine:latest"}' \
  http://localhost/v1/images/pull
```

`localhost` is only a placeholder HTTP host here; curl connects through the Unix
socket.

You are practicing:

- explicit dependency construction;
- signal-aware contexts;
- graceful HTTP shutdown;
- user-owned socket safety;
- cleanup order.

---

## Challenge 13: prove the real rootless path on Linux

macOS can compile packages and run unit tests, but it cannot prove `runc`
namespace behavior. Use the existing `lima-config.yaml` Linux VM for the final
smoke test.

Test this sequence as a normal user:

1. start `chamberd`;
2. confirm the Unix socket is mode `0600`;
3. pull `docker.io/library/alpine:latest`;
4. run `["/bin/sh", "-c", "id && echo chamber"]`;
5. inspect the container record in etcd;
6. confirm the state becomes `exited` and exit code becomes `0`;
7. confirm every created file is below the configured roots;
8. confirm operation and container records contain the expected correlation
   fields, even if trace and span IDs are empty in this phase;
9. stop the daemon and confirm the socket is removed.

Also run:

```sh
go test -race ./...
go vet ./...
```

The race detector validates Go memory access. It does not prove that filesystem
renames, etcd transactions, and runtime process transitions are correct, which
is why the Linux end-to-end check remains necessary.

---

## Questions to answer in your implementation notes

After each challenge, write two or three sentences answering the questions that
apply:

1. Which invariant does this package own?
2. What partial side effect remains if the next operation fails?
3. Can two requests execute this code concurrently?
4. What makes a record durable before an external process starts?
5. Which path does every created file use?
6. What can be recovered after the daemon crashes at this exact line?
7. Which identifiers correlate the durable record, trace, and log?
8. Which context value or durable field would a future observability adapter
   need here?

These questions connect the small Go exercises to the larger design pressures
in `plan.md`: a single authority, explicit state transitions, controlled paths,
concurrency, and eventual crash recovery.

## Phase 1 completion checklist

- The daemon listens only on its configured user-private Unix socket.
- Pull and run have strict request and response schemas.
- Every accepted pull and run has a durable operation ID returned to the client.
- Operation records are created before external side effects and transition
  atomically to terminal states.
- Image metadata includes reference, immutable digest, layout path, and pull
  time.
- Container metadata includes operation/trace correlation, image digest, image
  reference, bundle path, runtime, state, timestamps, exit code, and failure
  code.
- Every container state change is compare-and-swap.
- Metadata depends on `metadata.Store`, not directly on etcd.
- OCI execution depends on the two-method `runtime.Runtime` interface.
- The `runc` binary is pinned, downloaded into a configured directory, and
  checksum-verified.
- Images, containers, runtime state, runtime binaries, metadata, sockets, and
  temporary files all use explicit configurable paths.
- Unit tests do not require Docker Hub or a real runtime.
- The real pull-to-rootless-run path is verified on Linux without `sudo`.
- Structured logs carry operation and trace correlation without secrets,
  commands, environment variables, or request bodies.
- OpenTelemetry config fields exist, but Phase 1 does not initialize
  OpenTelemetry providers or require a collector.

At that point, stop. Logs, cancellation, deletion, leases, garbage collection,
and crash reconciliation are the next reliability layer; adding them here would
make this phase too broad to teach or review well. "Logs" here means the
container-log API: correlated daemon logs are part of this phase.
