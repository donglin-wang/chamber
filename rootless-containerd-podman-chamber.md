# Rootless container setup before `runc`

This note answers one narrow question:

> If `runc` can run rootlessly, why does a rootless container manager still need
> so much setup before calling `runc`?

The short answer is that `runc` starts the final container process. A container
manager has to prepare the world that process will see. Some of that preparation
asks the Linux kernel to do things normal users are not allowed to do.

In this note, "not configurable" means "not fixed by changing directories,
flags, or TOML values alone." Some problems can be solved by replacing the
mechanism, such as using a different storage backend or rootless network helper.
That is a real design choice, not simple configuration.

## First principles

A Linux container is just a process with a carefully prepared view of the
machine.

The kernel can be asked to make these promises to the process:

```text
This directory is your /
This process list is your process list
This network view is your network view
This user ID is root inside, but not root outside
These CPU and memory limits apply to you
```

`runc` knows how to ask the kernel to start that final process from an OCI
runtime bundle:

```text
bundle/
  config.json
  rootfs/
```

But `runc` does not usually pull the image, unpack it, stack filesystem layers,
create writable snapshots, configure rootless networking, or decide where daemon
state lives. Those are manager responsibilities.

That is why the hard parts happen before `runc`.

## Hard things containerd may do before `runc`

### 1. Build the container filesystem view

An image is not a directory. It is compressed layer data plus metadata:

```text
image:
  manifest
  config
  layer tar files
```

The container needs a real filesystem view:

```text
rootfs/
  bin/
  etc/
  lib/
  usr/
```

Containerd normally uses a snapshotter for this. A snapshotter prepares a
filesystem view from image layers and a writable top layer.

The efficient Linux mechanism is often an overlay mount:

```text
read-only layer
+ read-only layer
+ writable layer
= one merged root filesystem
```

The hard kernel operation is `mount`. A normal host user cannot usually create
overlay mounts. Changing containerd's data directory from `/var/lib/containerd`
to `$HOME/.local/share/containerd` does not grant mount permission.

What fails before `runc`:

```text
containerd snapshotter asks kernel to mount overlay filesystem
kernel says permission denied
```

Why config alone is not enough:

```text
Config can change where the layer data lives.
Config cannot make a normal user allowed to perform privileged mounts.
```

What must change instead:

```text
use a rootless-capable mount path
or use fuse-overlayfs
or use a copy-based storage path
or run the manager inside a user namespace
```

### 2. Preserve file ownership from image layers

Image layers contain ownership information:

```text
/bin/busybox is owned by uid 0
/etc/passwd is owned by uid 0
/var/lib/app is owned by uid 123
```

When unpacking the image, the manager may need to recreate that metadata.

The hard kernel operation is changing file ownership. A normal user cannot
usually say:

```text
make this file owned by root
```

even if the file is under that user's home directory.

What fails before `runc`:

```text
containerd unpack path tries to apply root-owned file metadata
kernel says permission denied
```

Why config alone is not enough:

```text
Config can choose the unpack directory.
Config cannot let a normal user create arbitrary root-owned files.
```

What must change instead:

```text
use user ID mappings
or unpack in a user namespace
or store shifted ownership
or ignore/changing ownership with known limitations
```

### 3. Create special filesystem entries

Some image or runtime setup may involve filesystem entries that are not ordinary
files:

```text
device nodes
some special permissions
some extended file attributes
some bind mounts
```

The hard kernel operations are things like creating device nodes or mounting one
path over another. Normal users are not generally allowed to do those on the
host.

What fails before `runc`:

```text
manager or runtime setup tries to create special filesystem state
kernel says permission denied
```

Why config alone is not enough:

```text
Config can avoid some entries.
Config cannot grant broad device or mount authority.
```

What must change instead:

```text
avoid those features
let runc create only safe minimal entries inside the container namespace
use rootless-compatible bind mount rules
or require host setup
```

### 4. Prepare networking

A container may expect its own network view:

```text
container has its own network interface
container has routes
container has DNS
container can reach the internet
host ports can reach container ports
```

A rootful manager can create virtual network devices and configure host routing.
A normal user cannot usually create host network devices or edit host firewall
and routing rules.

What fails before `runc`:

```text
network setup tries to create host network devices or routing rules
kernel says permission denied
```

Why config alone is not enough:

```text
Config can choose a network name or address range.
Config cannot let a normal user rewire host networking.
```

What must change instead:

```text
use user-mode networking
or use host networking
or skip separate container networking
or require admin-provided network setup
```

### 5. Apply CPU and memory controls

Linux uses cgroups to group processes and apply resource limits:

```text
this container can use this much memory
this container has this CPU weight
this process tree belongs to this container
```

A normal user can only control the parts of the cgroup tree that the system has
delegated to that user. Many hosts do not delegate enough by default.

What fails before or around `runc`:

```text
containerd asks for a cgroup path or resource limit
kernel says permission denied
```

Why config alone is not enough:

```text
Config can choose no limits or a different cgroup path.
Config cannot make the host delegate cgroup control to the user.
```

What must change instead:

```text
disable cgroup limits for the narrow path
or require cgroup v2 delegation
or rely on a user systemd session with delegation
```

### 6. Keep daemon, shim, and runtime state isolated per user

This is the part that is mostly configurable:

```text
/run/containerd
/var/lib/containerd
```

can become:

```text
$XDG_RUNTIME_DIR/containerd
$HOME/.local/share/containerd
```

But path configuration only solves where state is stored. It does not solve
mount permission, file ownership, networking authority, or cgroup delegation.

This is why saying "just configure containerd paths" is necessary but not
sufficient.

## How Podman handles these without root

Podman does not make the kernel rules disappear. It chooses rootless-aware
answers for each hard operation.

### 1. Identity: root inside, normal user outside

Podman uses user namespaces.

Plain version:

```text
inside container: uid 0
outside host:     your normal user
```

With wider ID ranges, the host can also map many container users to many
host-side subordinate IDs:

```text
inside container: uid 0..65535
outside host:     a user-owned delegated uid range
```

This lets files look root-owned inside the container without giving the process
real host root.

### 2. Storage: use rootless-capable storage

For the filesystem stack, Podman can use rootless-capable storage choices:

```text
kernel rootless overlay when available
fuse-overlayfs when needed
copy-based fallback when necessary
```

The important idea is that Podman does not ask a plain host user to perform the
same privileged overlay mount a rootful engine would perform. It chooses a
storage path that works with rootless constraints.

### 3. Image ownership: map or adapt ownership

When image layers say "this file is owned by root," Podman uses user namespace
mappings and rootless storage behavior so that ownership makes sense inside the
container without requiring real host root.

Some fallback modes can also ignore or squash ownership, but that has tradeoffs:
the container may not behave exactly like it would under a full UID/GID mapping.

### 4. Networking: use user-mode networking

Instead of creating host network devices and rewriting host routing as root,
Podman can use rootless networking helpers.

Plain version:

```text
packets from the container go through a normal user process
that process talks to the host network
```

This avoids giving the container manager permission to reconfigure host
networking.

### 5. Cgroups: use delegation or reduce the feature

If the host gives the user a delegated cgroup area, Podman can use it for
resource controls. If the host does not, some limits are unavailable or reduced.

Again, the rootless answer is not magic. It is:

```text
use the delegated control the host gives
otherwise do less
```

### 6. Process supervision: use user-owned helper processes

Podman uses helper processes to watch containers, collect exit status, and keep
stdio connected. These helpers run as the user, write to user-owned paths, and
avoid a root-owned central daemon for normal use.

## Why containerd commonly uses RootlessKit

Containerd is a manager daemon. If it runs as a plain host user, only the final
`runc` process is rootless-aware. The manager itself is still outside the
rootless environment while it prepares storage, mounts, networking, cgroups, and
runtime state.

RootlessKit moves the rootless environment earlier:

```text
host user
  -> RootlessKit creates user/mount/network setup
      -> containerd runs inside that setup
          -> containerd prepares container state
              -> runc starts the workload
```

RootlessKit is not the workload container. It is a wrapper that gives containerd
a private environment where more of containerd's normal assumptions can work
without real host root.

## How Chamber's current plan avoids the hard containerd path

Chamber's first rootless path is intentionally smaller than containerd or
Podman. It does not try to be a general container engine in the first MVP.

The current narrow path is:

```text
pull image
  -> write OCI image layout
      -> unpack into a real rootfs directory
          -> patch config.json for rootless runc
              -> call runc
```

### 1. Chamber avoids containerd's snapshot manager

Chamber does not ask containerd to create snapshots or overlay mounts.

For the first path, Chamber can unpack the image into a concrete directory:

```text
rootfs/
  bin/
  etc/
  lib/
  usr/
```

This is less efficient than a mature layer/snapshot system, but it removes a
large rootless storage problem from the first learning path.

Tradeoff:

```text
less efficient
less reusable layer storage
fewer mount problems
easier to understand
```

### 2. Chamber uses rootless unpacking behavior

The image still has to become a root filesystem. The first proof-of-concept path
uses `umoci` to turn an OCI image layout into:

```text
config.json
rootfs/
```

The rootless lesson is that the unpack step must not require real host root to
preserve every piece of root-owned metadata exactly as a rootful engine would.
For MVP 1, this is acceptable because the goal is to prove the pull-to-run path,
not to build the final high-performance storage layer.

### 3. Chamber passes rootless identity mapping to `runc`

Before calling `runc`, Chamber patches the OCI runtime config so that:

```text
container uid 0 -> daemon user's host uid
container gid 0 -> daemon user's host gid
```

Plain version:

```text
the process can think it is root inside
but it is still only the Chamber user's process on the host
```

This is the part where `runc` rootless support matters.

### 4. Chamber skips separate container networking in the first path

The MVP worksheet says to omit the network namespace for the first narrow
rootless path.

That means Chamber does not yet need to create virtual network devices, assign
container IP addresses, set up NAT, or publish ports.

Tradeoff:

```text
less isolation
no general container networking
much less rootless setup
```

This is a learning-stage limitation, not the finished design.

### 5. Chamber skips cgroup limits in the first path

The MVP worksheet says to clear cgroup path and resource settings for the first
narrow rootless path.

That means Chamber does not yet need delegated cgroup control to prove that a
rootless container process can start.

Tradeoff:

```text
no CPU or memory limits in the first narrow path
less host-specific setup
fewer permission failures before learning the basic runtime flow
```

### 6. Chamber owns its own user-private paths

Chamber still needs careful paths:

```text
image layout storage
container rootfs directories
runtime bundles
runc state root
temporary directories
logs
metadata database
Unix socket
```

But these are normal user-owned paths. They do not by themselves require root.

The important boundary is:

```text
Chamber path setup:
  ordinary files and directories owned by the daemon user

Containerd hard setup:
  mounts, root-owned metadata, networking, cgroups, and snapshot machinery
```

## Side-by-side summary

| Problem | Rootful containerd usually does | Podman rootless does | Chamber MVP 1 does |
| --- | --- | --- | --- |
| Layer filesystem | Kernel overlay mount through snapshotter | Rootless overlay, fuse-overlayfs, or fallback | Unpack to a real `rootfs` directory |
| Root-owned files | Preserve ownership as root | Map IDs through user namespaces or adapt storage | Use rootless unpacking and simple UID/GID mapping |
| Networking | Create network devices, routes, NAT, port rules | Use user-mode networking or reduced modes | Skip separate container networking first |
| Cgroups | Create cgroup paths and set resource limits | Use delegated cgroups or reduce limits | Clear cgroup settings first |
| Runtime process | Call `runc`/`crun` | Call `runc`/`crun` | Call `runc` directly |
| Daemon model | Central manager prepares everything | Mostly daemonless user workflow | One Chamber daemon owns only Chamber's narrow state |

## The key distinction

Rootless `runc` answers this question:

```text
Can this prepared bundle become a rootless container process?
```

Rootless containerd has to answer this larger question:

```text
Can this whole daemon prepare image storage, snapshots, mounts, networking,
cgroups, shims, runtime state, and then start a container without root?
```

Chamber MVP 1 deliberately asks the smaller question first:

```text
Can Chamber pull one image, prepare one simple bundle, patch it for rootless
execution, and start it with runc without sudo?
```

That is why Chamber can avoid much of the machinery that makes rootless
containerd difficult. It is not because the hard problems are fake. It is
because the MVP chooses not to solve all of them at once.

## References

- Containerd rootless mode:
  <https://github.com/containerd/containerd/blob/main/docs/rootless.md>
- Nerdctl rootless containerd workflow:
  <https://github.com/containerd/nerdctl/blob/main/docs/rootless.md>
- Podman rootless mode:
  <https://docs.podman.io/en/latest/markdown/podman.1.html#rootless-mode>
- Chamber plan:
  [`plan.md`](plan.md)
- Chamber MVP worksheet:
  [`mvp-1-implementation-challenges.md`](mvp-1-implementation-challenges.md)
