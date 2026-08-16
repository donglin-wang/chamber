# Host Assumption Validator Plan

Chamber's rootless promise depends on Linux host capabilities that Go code
cannot manufacture by itself. The SDK can manage paths, binaries, layouts, and
error shape, but Dockerfile builds and OCI runtime execution still rely on
kernel features, user mappings, runtime helpers, and filesystem behavior.

This note captures the assumptions that should become an explicit validator
before Chamber treats build/provision/run as point-and-shoot.

## First Principle

Rootless image build and rootless container run both need to create a process
that sees itself as privileged inside an isolated filesystem while remaining an
ordinary user on the host.

That requires:

- a Linux kernel that permits unprivileged user namespaces;
- enough UID/GID mapping to represent root and image-owned files safely;
- filesystem behavior that supports unpacking, overlaying, or copying image
  layers without host root;
- an OCI runtime path for executing isolated processes;
- writable private state owned by the calling user;
- predictable temp/runtime directories that are not shared across users.

BuildKit and runc hit many of the same assumptions because both eventually need
rootless namespaces and process execution. BuildKit adds builder state and
Dockerfile `RUN`; runc adds final container lifecycle/cgroup/log behavior.

## Shared Assumptions

- Host OS is Linux. macOS is only a validation/development environment unless
  Chamber is running inside a Linux VM.
- The current user can create user namespaces.
- The kernel exposes the namespaces Chamber needs: user, mount, pid, ipc, uts,
  and usually cgroup.
- The process can create private directories and temp files under Chamber roots.
- `XDG_RUNTIME_DIR` is usable or Chamber can provide an equivalent user-owned
  runtime directory where the underlying tools accept it.
- The root filesystem and Chamber roots allow executable files where executable
  helpers are needed.
- The host has enough disk space and inode capacity for image layers, bundle
  rootfs trees, logs, and temporary archives.
- The current user's environment is not relying on root-owned global container
  state.

## UID/GID Mapping

Rootless work needs two levels of identity:

- host identity: the real unprivileged user;
- namespace identity: often UID 0 inside the user namespace.

Validator checks:

- `/proc/sys/kernel/unprivileged_userns_clone` when present;
- `/proc/sys/user/max_user_namespaces` when present;
- Ubuntu AppArmor user namespace restriction sysctls when present:
  `/proc/sys/kernel/apparmor_restrict_unprivileged_userns` and
  `/proc/sys/kernel/apparmor_restrict_unprivileged_unconfined`;
- `/etc/subuid` and `/etc/subgid` entries for the current user;
- `newuidmap` and `newgidmap` availability when multiple ID ranges are needed;
- whether a minimal user namespace probe can map root inside the namespace;
- whether the available range is large enough for the intended image workload.

The minimum viable validator can distinguish:

- single-ID rootless mode: may run simple cases but cannot faithfully represent
  arbitrary image ownership;
- subordinate-ID mode: better default for Dockerfile builds and general OCI
  images;
- unavailable: Chamber should fail before build/run with a clear diagnostic.

On Ubuntu hosts, AppArmor can deny unprivileged user namespace creation even
when the generic namespace limits look usable. If either AppArmor restriction
is enabled for an unconfined Chamber process, rootless BuildKit or runc may
fail later with `Operation not permitted` during `unshare` or `runc init`. The
validator should report this as unsupported host policy and suggest either an
appropriate AppArmor profile for the Chamber entrypoint or an operator-owned
sysctl change, instead of treating it as an image, bundle, or runtime binary
problem.

## BuildKit Builder Assumptions

Chamber's BuildKit backend shells out to managed `buildctl`, `buildkitd`,
`rootlesskit`, and `runc` binaries. The image store starts an ephemeral
rootless BuildKit daemon for each build and keeps BuildKit state, runtime
directories, home/config directories, temporary files, and the output archive
under the image store root.

Validator checks:

- configured local BuildKit tool paths, when set, are absolute and not
  directories;
- managed BuildKit and RootlessKit archive URL, SHA256, and cache paths below
  `<image-root>/bin` are configured for the host architecture;
- managed runc URL, SHA256, and cache path below `<image-root>/bin/runc` are
  configured for the host architecture;
- existing tool executables answer a `--version` probe;
- the current user is non-root for the rootless BuildKit path;
- unprivileged user namespaces are enabled;
- AppArmor policy permits unprivileged user namespace setup;
- BuildKit's `native` snapshotter works as the conservative default;
- Chamber image root can hold BuildKit state/run/home/tmp directories;
- BuildKit can write a `type=oci,dest=<archive>` result into a Chamber temp
  directory;
- registry access, auth files, custom certs, and proxy settings are available
  when Dockerfiles use remote base images.

Useful active probe:

- build a tiny `FROM scratch` Dockerfile into a temporary OCI archive under a
  Chamber temp root.

Stronger active probe:

- build a tiny Dockerfile with a `RUN true` step. This verifies the expensive
  part: BuildKit can execute build containers rootlessly, not just assemble a
  scratch image.

## Bundle Provisioning Assumptions

The directory provisioner unpacks a selected image manifest into an OCI bundle
rootfs. It does not need BuildKit's snapshotter, but it still needs safe
rootless filesystem behavior.

Validator checks:

- Chamber bundle root is private and writable;
- temp directories can be created privately under the bundle root;
- image layout is readable by the current user;
- layer tar entries can be unpacked without escaping the bundle root;
- whiteouts, symlinks, hardlinks, file modes, and ownership mappings behave as
  expected for rootless unpack;
- the target filesystem supports the metadata needed by the provisioner.

Useful active probe:

- provision a known tiny local OCI layout into a temporary bundle and validate
  `config.json` plus rootfs existence.

## runc Runtime Assumptions

runc takes a provisioned bundle and starts the actual container process.

Validator checks:

- configured runc path exists, is executable, and reports a supported version;
- Chamber can download/install runc into the configured runtime bin directory
  when that path is managed;
- user namespaces are available for rootless specs;
- host policy permits the final `runc init` process to unshare the required
  namespaces; `Operation not permitted` at this stage usually means user
  namespaces, Ubuntu AppArmor userns restrictions, or another LSM/seccomp
  policy blocked rootless execution;
- cgroup mode is compatible with the selected runtime profile;
- cgroup v2 delegation is available if Chamber intends to set cgroup limits
  rootlessly;
- `XDG_RUNTIME_DIR` or Chamber runtime root is private and usable;
- pid, mount, ipc, uts, and cgroup namespaces can be created;
- seccomp, capabilities, and no-new-privileges behavior match the runtime
  profile Chamber will generate;
- logs can be created, appended, synced, read, and deleted under runtime-owned
  paths.

Useful active probe:

- run a pre-provisioned scratch or busybox bundle that exits immediately and
  verify wait status plus stdout/stderr log paths.

## Cross-Architecture Builds

`BuildRequest.Platform` can describe a target platform that differs from the
host. Building a foreign architecture image is not automatically equivalent to
running foreign architecture `RUN` instructions.

Validator checks:

- requested platform equals host platform, or;
- the host has usable binfmt/qemu support for the requested architecture, or;
- the Dockerfile path avoids architecture-specific `RUN` execution.

Without this, a cross-platform build may pull the right base image and then fail
on the first `RUN`.

## Validator Shape

The validator should be separate from build/provision/run constructors.
Constructors should validate Chamber config and private directories. The
validator should validate host capability and optionally run probes.

Suggested public shape:

```go
type CheckScope string

const (
    CheckBuild     CheckScope = "build"
    CheckProvision CheckScope = "provision"
    CheckRun       CheckScope = "run"
)

type CheckMode string

const (
    CheckStatic CheckMode = "static"
    CheckProbe  CheckMode = "probe"
)

type Finding struct {
    Scope       CheckScope
    Code        string
    Severity    string
    Message     string
    Remediation string
}

type Validator interface {
    Check(ctx context.Context, request CheckRequest) (Report, error)
}
```

Static checks should not mutate the host beyond reading files and inspecting
configured binaries. Probe checks may create temporary Chamber-owned directories,
build tiny images, provision temporary bundles, and run short-lived containers.

## Severity Model

- `fatal`: Chamber cannot perform the requested scope.
- `warning`: Chamber can try, but some Dockerfiles/images/runtime profiles may
  fail.
- `info`: host detail worth reporting for debugging.

The goal is not to hide platform complexity. The goal is to fail before the user
has to reverse-engineer a BuildKit, RootlessKit, or runc error.
