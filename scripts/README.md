# CI installation

Debian is implemented. There are two accounts: your normal SSH administrator (usually `admin` on Debian EC2) prepares the host; `chamber-ci` installs and runs CI without sudo.

## 1. Prepare the host as your SSH administrator

Run from your existing Chamber checkout. Go 1.26.4 or newer must already be installed somewhere the CI account can access, such as `/usr/local/go`. The scripts do not install Go.

```sh
sudo ./scripts/setup-host.sh
```

Host setup installs Debian packages and creates `chamber-ci` using `useradd`, which automatically allocates unused ranges of one million subordinate UIDs and GIDs. It enables user namespaces and user lingering for boot and logout survival. Existing accounts and allocations are preserved. Reruns reuse the CI account if its first UID and GID allocations are already large enough. Optional: to use a different CI account name, replace the command above with `sudo ./scripts/setup-host.sh my-ci`, and use `my-ci` in the switch command below. Run this alternative as your SSH administrator, before switching accounts.

## 2. Switch from your SSH administrator to the CI account

```sh
sudo -iu chamber-ci
```

You are now `chamber-ci`. All remaining installation commands run as this user, which has no sudo privileges and does not need them. Use `exit` to return to your SSH administrator if you later need host administration.

## 3. Install CI as the CI account

Obtain a checkout owned by this account and put the existing Go installation on PATH. For Go installed under `/usr/local/go`:

```sh
export PATH="/usr/local/go/bin:$PATH"
git clone https://github.com/donglin-wang/chamber.git ~/chamber
cd ~/chamber
./scripts/install-ci.sh
```

User installation uses the existing Go on PATH to build this checkout and installs the canonical `cmd/ci/ci.service`. A PATH drop-in includes that Go executable's directory for self-updates. The build uses your Go toolchain configuration, including Go's normal automatic toolchain selection. Existing config and secrets are preserved. It refuses to replace a running CI binary; use CI self-update for normal updates. It does not change the login shell PATH.

The installer connects to the CI user's systemd manager without requiring an SSH login as that user.

## 4. Configure and start CI as the CI account

Edit `~/.config/chamber-ci/config.json`: set `public_url`, repository names, and the trusted update source. Paths are absolute; the generated `work_root` is `~/ci-work` expanded at installation, on persistent storage rather than `/tmp`. Keep that path short because BuildKit uses Unix sockets. Edit `~/.config/chamber-ci/secrets.env` to supply a GitHub token with commit-status write permission. A random webhook secret is generated on first installation. Private repository checkout separately needs noninteractive Git credentials.

```sh
XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user enable --now chamber-ci
curl --fail http://127.0.0.1:8080/health
XDG_RUNTIME_DIR=/run/user/$(id -u) journalctl --user -u chamber-ci -f
```

Use your SSH administrator account to configure an HTTPS proxy and the EC2 security group; keep port 8080 private. Configure GitHub's JSON webhook at `https://YOUR_DOMAIN/webhooks/chamber` (or your chosen repository key), using the generated secret and push/pull request events. See [the CI guide](../cmd/ci/README.md) for credentials, logs, and self-update.

## Adding a distribution

`setup-host.sh` dispatches on `/etc/os-release`'s exact `ID`. Ubuntu, Fedora/RHEL (including Rocky and AlmaLinux), and Arch have explicit unimplemented branches; they fail before host setup rather than assuming Debian compatibility.

Copy `distros/template.sh` to `distros/<ID>.sh`, implement `install_host_packages`, and replace the corresponding dispatch branch with a source of that file. Keep package names and any distro-specific LSM/user namespace policy in that adapter. The shared scripts own ID allocations, sysctls, lingering, application files, and the user service. Validate on the target distro, including mapping helpers and AppArmor/SELinux policy, before marking it supported. Host setup does not disable an LSM or apply container-specific privilege workarounds.

