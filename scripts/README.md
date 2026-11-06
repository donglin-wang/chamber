# CI installation

Debian is implemented. Run these from a checkout as the existing non-root SSH
user that will own CI. Install Go 1.26.4 or newer yourself and put `go` on PATH
before running the user installer (Git is needed to obtain the checkout):

```sh
sudo ./scripts/setup-host.sh "$(id -un)"
./scripts/install-ci.sh
```

If the user systemd session is unavailable after installing its packages,
disconnect and reconnect over SSH, then rerun the second command.

Host setup installs Debian packages, enables unprivileged user namespaces,
sets the user namespace limit to 28,633, and enables user lingering for boot
and logout survival. It replaces a small first subordinate UID/GID allocation
with one million IDs, preserving a first range that already has that size.
Use this for initial host provisioning, before that user has rootless workloads
or mapped files depending on the old ranges. Existing allocations belonging to
other users and real host accounts are checked for overlap before any range is
changed. To select a different free block:

```sh
sudo ./scripts/setup-host.sh "$(id -un)" 10000000
```

User installation uses the existing Go on PATH to build this checkout and
installs the canonical `cmd/ci/ci.service`. A PATH drop-in includes that Go
executable's directory for self-updates. The build disables automatic Go
toolchain downloads; an incompatible installed version fails the build.
Existing config and secrets are
preserved. It refuses to replace a running CI binary; use CI self-update for
normal updates. It does not change the login shell PATH.

Edit `~/.config/chamber-ci/config.json`: set `public_url`, repository names, and
the trusted update source. Paths are absolute; the generated `work_root` is
`~/ci-work` expanded at installation, on persistent storage rather than `/tmp`.
Keep that path short because BuildKit uses Unix sockets. Edit
`~/.config/chamber-ci/secrets.env` to supply a GitHub token with commit-status
write permission. A random webhook secret is generated on first installation.
Private repository checkout separately needs noninteractive Git credentials.

```sh
systemctl --user enable --now chamber-ci
curl --fail http://127.0.0.1:8080/health
journalctl --user -u chamber-ci -f
```

Put HTTPS in front of the loopback listener and allow public inbound traffic
only to the proxy, keeping port 8080 private. Configure GitHub's JSON webhook
at `https://YOUR_DOMAIN/webhooks/chamber` (or your chosen repository key), using
the generated secret and push/pull request events. See
[the CI guide](../cmd/ci/README.md) for credentials, logs, and self-update.

## Adding a distribution

`setup-host.sh` dispatches on `/etc/os-release`'s exact `ID`. Ubuntu,
Fedora/RHEL (including Rocky and AlmaLinux), and Arch have explicit unimplemented
branches; they fail before host setup rather than assuming Debian compatibility.

Copy `distros/template.sh` to `distros/<ID>.sh`, implement
`install_host_packages`, and replace the corresponding dispatch branch with a
source of that file. Keep package names and any distro-specific LSM/user namespace
policy in that adapter. The shared scripts own ID allocations, sysctls,
lingering, application files, and the user service. Validate on the target
distro, including mapping helpers and AppArmor/SELinux policy, before marking it
supported. Host setup does not disable an LSM or apply container-specific
privilege workarounds.

