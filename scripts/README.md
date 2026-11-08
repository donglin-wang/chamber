# Rootless Linux host preparation

`setup-host.sh` restores the host setup from commit `c18f4d1`, without the removed
CI application installer. Its package adapters cover Debian, Ubuntu, Fedora,
Rocky Linux 10+, and AlmaLinux 10+. Setup checks that `unshare` supports the
required mapping flags; older Rocky/AlmaLinux 9 packages do not. AlmaLinux 10
shares the package adapter but has not been separately validated.

An administrator runs this once from the checkout:

```sh
sudo ./scripts/setup-host.sh chamber
```

On Ubuntu with restricted unprivileged user namespaces, first review the policy
change, then explicitly permit it:

```sh
sudo ./scripts/setup-host.sh --allow-unrestricted-userns chamber
```

That flag writes system-wide AppArmor user-namespace restriction sysctls to zero;
it does not disable AppArmor itself. The flag is intended for dedicated execution
hosts. Without it the script stops before changing a restricted host. SELinux
remains enabled on Fedora/Rocky/AlmaLinux.

The script installs distro mapping helpers, creates an unprivileged account with
one million subordinate UIDs/GIDs, enables user namespaces, and verifies namespace
creation. SDK execution does not require systemd user services or a D-Bus daemon;
BuildKit creates its own private runtime directory. The script never rewrites an
existing account's ID allocations or grants sudo access. Reruns require the
account's first ranges to be large enough. Choose another account if an existing
account has shorter ranges.
Install Go separately at the version required by `go.mod`; setup does not install Go.

Switch with `sudo -iu chamber`. All SDK operations and tests run as that user,
with a user-owned checkout on a local Linux filesystem:

```sh
export PATH=/usr/local/go/bin:$PATH
export TMPDIR="$HOME/tmp"
mkdir -p "$TMPDIR"
CHAMBER_INTEGRATION=1 go test -count=1 -timeout=20m ./pkg/...
```

CircleCI runs `.circleci/test-host.sh` directly on native Ubuntu 24.04 and
26.04 machine executors. It runs setup twice, checks allocation stability, copies
the executor's existing Go toolchain and source into a fresh account, then runs
the SDK suite and compiler example without sudo. There is no nested VM launcher.
CircleCI Cloud's [hosted Linux VM images](https://circleci.com/docs/guides/execute/linux-vm-support-policy/)
are Ubuntu-based; native Debian/Fedora/Rocky jobs require self-hosted runners.
The existing nested-container SDK job remains separate.

Debian 13 may mount `/tmp` with `nosuid`. Keep bundle roots and temporary roots on
a filesystem that permits file capabilities when using nested mapping helpers.
The VM tests use `$HOME/tmp` for this reason. Keep temporary paths short because
BuildKit uses Unix sockets with a bounded path length.
