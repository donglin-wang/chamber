# Host Filesystem Workspace Implementation Plan

This is a handoff plan for implementing Chamber's next filesystem boundary.
Keep the implementation minimal, but make the guarantees explicit.

## Goal

Replace `pkg/shared/localfs.DirectoryManager` with a scoped host-filesystem
workspace:

```go
pkg/shared/hostfs
type Workspace struct
```

Each package gets its own `hostfs.Workspace`. All durable package directories
are created below that package's durable root. All scratch temporary directories
are created below that package's configured temporary root. The temporary root
must satisfy every atomic rename relationship the package requests: within the
temporary root, within the durable root, and across temporary root to durable
root.

Do not preserve backward compatibility.

## Design

Create `pkg/shared/hostfs`.

```go
type Config struct {
	Root         string
	TmpRoot      string
	Capabilities Capabilities
}

type Workspace struct {
	root    string
	tmpRoot string
	caps    Capabilities
}

type Capabilities struct {
	PrivateDirs           bool
	FileFsync             bool
	DirectoryFsync        bool
	AtomicFileRename      bool
	AtomicDirectoryRename bool
}
```

Public constructor:

```go
func NewWorkspace(config Config) (*Workspace, error)
```

Public methods:

```go
func (w *Workspace) Root() string
func (w *Workspace) TmpRoot() string
func (w *Workspace) Capabilities() Capabilities

func (w *Workspace) MkdirPrivate(relDir string) (string, error)
func (w *Workspace) CreatePrivate(rel string) (*os.File, error)
func (w *Workspace) MkdirTemp(relDir string, pattern string) (string, error)
func (w *Workspace) CreateTemp(relDir string, pattern string) (*os.File, error)
```

Skip `MkdirParent` for now. Add it later only if real callers need it.
Keep file creation narrow. `CreatePrivate` exists for package-owned durable
files that only need Chamber's default private-file creation policy. Add a more
general `OpenFile`-style method later only when a real caller needs flags or
custom modes.

`MkdirPrivate` should roughly preserve the current
`localfs.DirectoryManager.MkdirPrivate` behavior: create missing directories
with `0700`, accept existing directories only when they are directories owned
by the current effective user, and reject group/other-accessible paths when
`PrivateDirs` is required. `CreatePrivate` should follow the same permission
model for files: create the file below `Root`, fail if it already exists, use a
private file mode such as `0600`, and reject unsafe existing paths rather than
loosening permissions.

## Path Rules

- `Config.Root` is required.
- `Config.Root` must resolve to an absolute path.
- Empty `Config.TmpRoot` defaults to a private directory below `os.TempDir()`,
  for example `<os.TempDir()>/chamber-<uid>`.
- `Config.TmpRoot`, when set, must resolve to an absolute path.
- `MkdirPrivate`, `CreatePrivate`, `MkdirTemp`, and `CreateTemp` accept
  package-relative paths.
- Reject empty relative paths except `.` where it is useful for the package
  root or temp root itself.
- Reject absolute relative-path arguments.
- Reject paths that escape the workspace root with `..`.
- When `PrivateDirs` is requested, create and validate Chamber-owned
  directories as private: directory, current effective user owner, and no group
  or other permission bits.
- Keep package layout knowledge out of `hostfs`. Image, bundle, runtime, and
  metadata packages still choose names like `layout`, `metadata`, `logs`, and
  `blobs`.

## Workspace Initialization Checks

`Config.Capabilities` is the set of filesystem guarantees the caller requires
for this workspace. `NewWorkspace` should create and validate `Root` and
`TmpRoot`, probe the actual filesystem behavior, compare the observed
capabilities against the requested capabilities, and fail if a required
capability is missing.

### 1. Filesystem Capability Probing

Probe below the actual workspace root, not in a generic temp location.

At minimum:

- Create a probe directory below `Root`.
- Create a temporary file in that probe directory.
- Write bytes to the file.
- `Sync` the file.
- Rename the file within `Root`.
- Rename the file within `TmpRoot`.
- Rename the file from `TmpRoot` into `Root`.
- Create a temporary directory in the probe directory.
- Rename the temporary directory within `Root`.
- Rename the temporary directory within `TmpRoot`.
- Rename the temporary directory from `TmpRoot` into `Root`.
- Attempt to fsync the probe directory.
- Remove the probe directory.

Store observed capability results in `Workspace.caps`.

Failure policy:

- If private directory creation/validation fails and
  `Config.Capabilities.PrivateDirs` is true, constructor fails.
- If file fsync fails and `Config.Capabilities.FileFsync` is true,
  constructor fails.
- If any file rename probe fails and `Config.Capabilities.AtomicFileRename` is
  true, constructor fails. This includes file rename within `Root`, within
  `TmpRoot`, and from `TmpRoot` into `Root`.
- If any directory rename probe fails and
  `Config.Capabilities.AtomicDirectoryRename` is true, constructor fails. This
  includes directory rename within `Root`, within `TmpRoot`, and from `TmpRoot`
  into `Root`.
- If directory fsync is unsupported on the platform/filesystem and
  `Config.Capabilities.DirectoryFsync` is true, constructor fails.
- If a capability is not requested, record the observed result but do not fail
  solely because it is absent.

### 2. Directory Fsync Portability

Add an unexported helper:

```go
func syncDirectory(path string) error
```

Expected behavior:

- On platforms/filesystems where opening and syncing a directory works, do it.
- If the platform returns a known unsupported error for directory fsync, return
  an internal sentinel such as `errDirectorySyncUnsupported`.
- Do not silently ignore unexpected errors.
- Capability probing should convert the sentinel into
  `DirectoryFsync=false`.

Use this helper later in atomic write/publish code. For this plan, the
constructor only needs to probe and record the capability.

### 3. Temp Rename Safety

Rules:

- `MkdirTemp` and `CreateTemp` allocate all package temporary entries below
  `TmpRoot`.
- A caller may use a temp entry as durable commit input only when the workspace
  was constructed with the relevant atomic rename capability.
- `AtomicFileRename` requires successful file rename probes in all three
  relationships: within `TmpRoot`, within `Root`, and from `TmpRoot` into
  `Root`.
- `AtomicDirectoryRename` requires successful directory rename probes in all
  three relationships: within `TmpRoot`, within `Root`, and from `TmpRoot` into
  `Root`.
- This startup validation is what prevents cross-filesystem `EXDEV` for
  packages that use temp paths as commit staging paths.

## Error Style

Use existing Chamber error taxonomy from `pkg/shared/errors`.

Suggested mapping:

- Invalid empty or escaping paths: `ErrInvalidRequest`.
- Required private directory permission or ownership violation:
  `ErrInvalidRequest`.
- Host filesystem operation failure: `ErrFilesystemFailed`.
- Unsupported required filesystem capability: `ErrFilesystemFailed` with a
  clear message explaining the failed capability.

Prefer errors that name both the operation and the path.

## Migration Steps

1. Add `pkg/shared/hostfs`.
2. Move and adapt tests from `pkg/shared/localfs/directory_test.go`.
3. Delete `pkg/shared/localfs` once callers are migrated.
4. Replace `localfs.DirectoryManager` parameters with `*hostfs.Workspace` or a
   narrow interface where tests need one.
5. Update daemon composition to create one workspace per package:

```go
imageWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
	Root:    cfg.Image.Root,
	TmpRoot: filepath.Join(cfg.TmpRoot, "images"),
	Capabilities: hostfs.Capabilities{
		PrivateDirs:           true,
		FileFsync:             true,
		AtomicFileRename:      true,
		AtomicDirectoryRename: true,
	},
})

bundleWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
	Root:    cfg.Bundle.Root,
	TmpRoot: filepath.Join(cfg.TmpRoot, "bundles"),
	Capabilities: hostfs.Capabilities{
		PrivateDirs:           true,
		AtomicDirectoryRename: true,
	},
})

runtimeWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
	Root:    cfg.Runtime.RuntimeRoot,
	TmpRoot: filepath.Join(cfg.TmpRoot, "runtime"),
	Capabilities: hostfs.Capabilities{
		PrivateDirs:      true,
		FileFsync:        true,
		AtomicFileRename: true,
	},
})

metadataWorkspace, err := hostfs.NewWorkspace(hostfs.Config{
	Root:    cfg.Metadata.Root,
	TmpRoot: filepath.Join(cfg.TmpRoot, "metadata"),
	Capabilities: hostfs.Capabilities{
		PrivateDirs:      true,
		FileFsync:        true,
		AtomicFileRename: true,
		DirectoryFsync:   true,
	},
})
```

6. Update image store setup:

- `MkdirPrivate("layout")`
- `MkdirPrivate("metadata")`
- `MkdirPrivate("metadata/images")`
- Pull output layouts and build output layouts that will be committed into the
  shared image layout use `MkdirTemp`. The workspace constructor must validate
  `AtomicDirectoryRename` when those temp layouts are renamed or otherwise
  used as directory commit staging.
- BuildKit daemon state, sockets, HOME, and other disposable process scratch
  dirs may use `MkdirTemp` below `TmpRoot`.
- Atomic `index.json` and blob writes use `CreateTemp`, then rename into the
  final parent below `Root`. The workspace constructor must validate
  `AtomicFileRename` for packages that do this.

7. Update bundle provisioning:

- The final bundle remains below bundle root.
- Temporary unpack bundle uses `MkdirTemp(".", "."+containerID+".tmp-*")`,
  then renames into the final bundle path below `Root`. The bundle workspace
  must require `AtomicDirectoryRename`.
- Keep `os.Rename(tmpBundle, finalBundle)`.

8. Update runtime:

- Runtime logs are durable and should use
  `MkdirPrivate("logs/<container-id>")` or equivalent package-owned relative
  paths, then `CreatePrivate` for the log files.
- Runtime binary downloads use `CreateTemp`, then rename into the runtime
  binary directory. If runtime binaries remain outside `RuntimeRoot`, either
  create a separate `hostfs.Workspace` for the bin dir or first move binaries
  below the runtime package root.

9. Update metadata:

- Metadata durable files use `CreateTemp`, then rename into metadata-owned
  directories below `Root`.

10. Update tests and failing test doubles.

## Tests To Add

In `pkg/shared/hostfs`:

- `NewWorkspace` creates root and temp root.
- `NewWorkspace` validates private root and temp root when `PrivateDirs` is
  requested.
- Empty `TmpRoot` defaults below `os.TempDir()`.
- Existing unsafe root is rejected.
- Existing unsafe temp root is rejected.
- Relative path escaping is rejected.
- Absolute relative path argument is rejected.
- `MkdirPrivate("layout")` creates below root.
- `MkdirTemp(".", ".pull-*")` creates below tmp root.
- `CreateTemp(".", ".archive-*")` creates below tmp root.
- `MkdirTemp("pulls", ".pull-*")` creates below `<TmpRoot>/pulls`.
- `CreatePrivate("logs/container/stdout.log")` creates below root.
- `CreateTemp("layout", ".index.tmp-*")` creates below `<TmpRoot>/layout`.
- `MkdirTemp(".", ".bundle.tmp-*")` creates below `<TmpRoot>`.
- Capability probe records directory fsync support or unsupported status.
- Constructor fails when required file rename within `Root` fails.
- Constructor fails when required file rename within `TmpRoot` fails.
- Constructor fails when required file rename from `TmpRoot` into `Root` fails.
- Constructor does not fail when an unrequested file rename probe fails.
- Constructor fails when required directory rename within `Root` fails.
- Constructor fails when required directory rename within `TmpRoot` fails.
- Constructor fails when required directory rename from `TmpRoot` into `Root`
  fails.
- Constructor does not fail when an unrequested directory rename probe fails.
- Constructor fails when required directory fsync is unsupported.
- Constructor does not fail when unrequested directory fsync is unsupported.
- `MkdirTemp` and `CreateTemp` allocate below `TmpRoot`.

Where practical, use injected low-level operations for probe failure tests
rather than trying to find a real broken filesystem in unit tests.

In package tests:

- Image pull/build output layouts that feed shared-layout commits use temp
  paths and require `AtomicDirectoryRename`.
- Image atomic index/blob temp files use temp paths and require
  `AtomicFileRename`.
- Bundle temp unpack directory uses temp paths and requires
  `AtomicDirectoryRename`.
- BuildKit temp honors configured BuildKit temp root where that existing
  contract still applies.

## Validation

Run focused tests first:

```sh
GOCACHE=/tmp/chamber-go-cache go test ./pkg/shared/hostfs ./pkg/image/... ./pkg/bundle/... ./pkg/runtime/...
```

Then run broader tests:

```sh
GOCACHE=/tmp/chamber-go-cache go test ./...
```

In the restricted sandbox, listener or keychain failures may require rerunning
outside the sandbox. Do not treat native macOS runtime unsupported-host failures
as Linux runtime validation.

## Non-Goals

- Do not add cleanup, leases, recovery, or daemon GC to `hostfs`.
- Do not add NFS or overlayfs fallback behavior yet.
- Do not silently emulate atomic rename with copy/delete.
- Do not put package layout knowledge in `hostfs`.
- Do not broaden this into a general-purpose filesystem utility package.
