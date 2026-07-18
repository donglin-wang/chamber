# SDK Capabilities Hardening Plan

## Goal

Harden the public SDK packages under `pkg/` so each concrete implementation can
declare what it supports, validate selected configuration early, and leave room
for future implementations without baking in today's rootless-only assumptions.

This is a planning note for a fresh agent. Do not treat it as a request to add a
general plugin system yet.

## Current Context

The current public SDK shape is:

- `pkg/image`: image pull contract and shared image request/result types.
- `pkg/image/puller`: current concrete puller using `go-containerregistry`.
- `pkg/bundle`: bundle provisioning contract and shared bundle request/result
  types.
- `pkg/bundle/directory`: current concrete provisioner using `umoci`; it is
  named for the directory-backed provisioning mechanism rather than privilege.
- `pkg/runtime`: runtime contract and shared runtime request/result types.
- `pkg/runtime/runc`: current concrete runtime adapter.
- `pkg/shared/...`: shared public support packages.

The hardening work should preserve the interface/adapter split:

- Interface packages define public contracts and domain-specific support types.
- Concrete adapter packages declare their support and enforce their own limits.
- Shared packages hold common vocabulary only when the term has one meaning
  across SDK packages.

## Design Principles

1. Share vocabulary, not one universal capability struct.
2. Keep package-specific support declarations in the package that owns the
   interface.
3. Separate static implementation support from host readiness.
4. Validate impossible configuration early, preferably in constructors.
5. Validate request-specific unsupported behavior close to the operation that
   sees the request.
6. Do not make privilege mode the package name for bundle implementations.
7. Do not name a bundle implementation `oci`; every bundle provisioner should
   produce an OCI runtime bundle.

## Proposed Vocabulary Package

Create a small shared vocabulary package:

```text
pkg/shared/capability
```

The package name is intentionally singular, following common Go package style.
It should define reusable vocabulary terms, not a universal capability model.

Initial types:

```go
package capability

type Privilege string

const (
	Rootless Privilege = "rootless"
	Rootful  Privilege = "rootful"
)
```

Do not add platform support declarations here. Chamber is expected to grow a
separate `machine` concept for runs that happen inside an isolated VM, so
host/platform readiness should belong to that machine layer rather than every
SDK descriptor.

Add `Isolation` here only if more than one SDK package needs it. Runtime needs
it first, but bundle may not. Prefer starting with runtime-local isolation:

```go
package runtime

type Isolation string

const (
	ProcessIsolation Isolation = "process"
	VMIsolation      Isolation = "vm"
)
```

Decision point for the implementing agent: if `capability.Rootless` reads too
oddly during implementation, pause and confirm whether to rename the shared
package to `system`, `environment`, or `target`. Do not scatter duplicate
`Privilege` definitions while deciding.

## Runtime Support Design

Add package-owned support types to `pkg/runtime`.

Sketch:

```go
package runtime

import "github.com/donglin-wang/chamber/pkg/shared/capability"

type Isolation string

const (
	ProcessIsolation Isolation = "process"
	VMIsolation      Isolation = "vm"
)

type Capabilities struct {
	Privileges []capability.Privilege
	Isolation  []Isolation
}

type Descriptor struct {
	Name         string
	Version      string
	Capabilities Capabilities
}
```

Then extend the runtime interface:

```go
type Runtime interface {
	Descriptor() Descriptor
	Binary() Binary
	Run(ctx context.Context, request RunRequest) (Process, error)
	State(ctx context.Context, containerID string) (ContainerState, error)
	Signal(ctx context.Context, request SignalRequest) error
	Delete(ctx context.Context, request DeleteRequest) error
	ReadLog(containerID string, stream string) ([]byte, error)
}
```

Expected `pkg/runtime/runc` support:

```go
runtime.Descriptor{
	Name:    "runc",
	Version: configuredOrDetectedVersion,
	Capabilities: runtime.Capabilities{
		Privileges: []capability.Privilege{
			capability.Rootless,
			// capability.Rootful can be added only when implemented/tested.
		},
		Isolation: []runtime.Isolation{
			runtime.ProcessIsolation,
		},
	},
}
```

Do not overstate runtime support. If current Chamber only wires `runc` in a
rootless mode today, either:

- declare only `Rootless` for now; or
- declare both as theoretical adapter support and add a separate configured
  mode field.

Prefer the first option unless rootful operation is actually implemented and
tested.

## Bundle Support Design

Add package-owned support types to `pkg/bundle`.

Sketch:

```go
package bundle

import "github.com/donglin-wang/chamber/pkg/shared/capability"

type Capabilities struct {
	Privileges []capability.Privilege

	// Add later only when real implementations need these dimensions.
	// Mounts      MountCapabilities
	// Filesystems []Filesystem
}

type Descriptor struct {
	Name         string
	Version      string
	Capabilities Capabilities
}
```

Then extend the bundle interface:

```go
type Provisioner interface {
	Descriptor() Descriptor
	Provision(ctx context.Context, request ProvisionRequest) (ProvisionedBundle, error)
}
```

The current bundle implementation lives in `pkg/bundle/directory` because it
describes the provisioning mechanism rather than the selected privilege mode.

This means "provision an OCI bundle whose container filesystem is materialized
as a normal on-disk directory." It can later support:

- rootless + directory;
- rootful + directory.

Future sibling packages could be:

```text
pkg/bundle/fuseoverlay
pkg/bundle/overlayfs
pkg/bundle/zfs
```

Those packages should each declare supported privileges:

- `directory`: rootless first; rootful later if implemented.
- `fuseoverlay`: likely rootless.
- `overlayfs`: rootful only.
- `zfs`: rootful only.

If an implementation does not support a selected privilege, reject it in its
constructor or config validation path before any filesystem mutation.

## Validation Layers

Use four distinct validation concepts.

### 1. Static implementation declaration

Exposed through `Descriptor().Capabilities`.

This answers:

- What privilege modes can this implementation support in principle?
- For runtime, what isolation model does it provide?

This should not inspect the host.

### 2. Config validation

Constructor-time or explicit config validation.

This answers:

- Did the caller select a privilege mode this implementation does not support?
- Did the caller provide required paths, binary settings, pool names, or other
  implementation-specific config?

Example:

```go
overlayfs.New(overlayfs.Config{
	Privilege: capability.Rootless,
})
```

should fail quickly because kernel overlayfs should not be modeled as a
rootless Chamber bundle provisioner.

### 3. Host check

Optional explicit method or package function.

This answers:

- Is this host running in the expected machine context?
- Is the runtime binary executable here?
- Are user namespaces available?
- Is overlayfs available and mountable?
- Does the configured zfs pool exist?

Suggested shape:

```go
func CheckHost(ctx context.Context, config Config) error
```

Do not conflate this with static support. `overlayfs` can statically support
rootful Linux while still failing `CheckHost` on a particular Linux machine.

### 4. Request validation

Operation-time validation.

This answers:

- Does this specific request ask for a feature the implementation cannot apply?
- Does the runtime support the `ProvisionedBundle.RootFS.Mounts` handoff?
- Are requested bind mounts valid for the selected implementation?

Keep request validation close to the implementation that understands the
operation. For example, current rootless bind mounts are provisioner-owned and
injected into `config.json`; do not route that work through runtime-applied
`RootFS.Mounts` unless the runtime adapter explicitly supports it.

## Suggested Implementation Phases

### Phase 1: Shared vocabulary

- Add `pkg/shared/capability`.
- Define `Privilege`, `Rootless`, and `Rootful`.
- Add focused tests for string values and any helper functions if helpers are
  introduced.
- Do not add a generic `capability.Capabilities` struct.

### Phase 2: Runtime descriptors

- Add `runtime.Capabilities` and `runtime.Descriptor`.
- Add `Descriptor() Descriptor` to `runtime.Runtime`.
- Implement it in `pkg/runtime/runc`.
- Update tests to assert `runc` declares the intended privilege and isolation
  support.
- Keep host checks separate from descriptor support.

### Phase 3: Bundle descriptors

- Add `bundle.Capabilities` and `bundle.Descriptor`.
- Add `Descriptor() Descriptor` to `bundle.Provisioner`.
- Implement it in the current bundle implementation.

### Phase 4: Bundle implementation naming

- Keep the mechanism-based `pkg/bundle/directory` package name while preserving
  the implementation's rootless behavior.
- Update imports and aliases at composition boundaries.
- Keep docs clear that it produces an OCI runtime bundle, but do not use `oci`
  as the package name.
- Preserve existing rootless spec patching and bind mount behavior.

### Phase 5: Config validation

- Add privilege selection to relevant config structs only where the
  implementation actually needs it.
- Reject unsupported privilege/config combinations before filesystem mutation.
- Add tests for unsupported combinations, especially future rootful-only
  implementations.

### Phase 6: Host checks

- Add host checks only after there is a real caller that needs them, likely
  daemon startup validation or `chamber system info`.
- Keep checks implementation-specific.
- Avoid making constructors perform expensive probing unless the package already
  does expensive setup such as binary download/verification.

## Testing Guidance

Use focused unit tests first:

- shared vocabulary values;
- descriptor contents;
- constructor/config rejection for unsupported combinations;
- request validation behavior.

Start local verification with:

```sh
GOCACHE=/tmp/chamber-go-cache go test -run '^$' ./pkg/...
GOCACHE=/tmp/chamber-go-cache go test ./pkg/...
```

Some listener-backed or runtime-backed tests may fail in the sandbox with bind
or platform errors. Treat host/runtime smoke tests separately from compile and
unit-test verification.

## Open Questions For The Implementing Agent

1. Should the shared vocabulary package be `pkg/shared/capability`, `system`,
   `environment`, or `target`?
2. Should `runtime.Isolation` stay runtime-local until another package needs
   it?
3. Does current `runc` support declaration include only currently wired
   rootless support, or both rootless/rootful as adapter-level intent?
4. Should bundle package rename happen before descriptors, or after descriptors
   land with minimal churn?
5. Should host checks be added now, or deferred until daemon config validation
   has a concrete use for them?

## Non-Goals

- Do not introduce a containerd-style plugin system in this pass.
- Do not add `v0` or `v1` folders under each package.
- Do not add a universal shared `Capabilities` struct for all SDK packages.
- Do not move daemon reliability concepts into low-level SDK packages.
- Do not implement overlayfs, fuse-overlayfs, zfs, or VM runtimes as part of
  this hardening plan unless explicitly requested.
