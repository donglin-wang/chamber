#!/usr/bin/env bash
set -euo pipefail

if [[ ${1:-} == --help ]]; then
    echo "Usage: $0"
    echo "Run as the CI user after sudo scripts/setup-host.sh."
    echo "Uses Go from PATH to build this checkout and create config and a user service."
    exit 0
fi
[[ $# == 0 && $(uname -s) == Linux && $EUID != 0 ]] || {
    echo "Run without arguments as the non-root CI user on Linux." >&2; exit 1;
}
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(dirname -- "$script_dir")
command -v go >/dev/null || { echo "Install Go first and put it on PATH (see go.mod for the required version)." >&2; exit 1; }
go_bin_dir=$(cd -- "$(dirname -- "$(command -v go)")" && pwd)
for tool in git unshare newuidmap newgidmap systemctl; do
    command -v "$tool" >/dev/null || { echo "Missing $tool; run setup-host.sh first." >&2; exit 1; }
done
unshare -Ur true
# sudo -iu does not create a login session; lingering starts this user's manager.
export XDG_RUNTIME_DIR=/run/user/$UID
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
systemctl --user show-environment >/dev/null || {
    echo "User systemd is unavailable; run setup-host.sh for this CI account first." >&2; exit 1;
}
if systemctl --user is-active --quiet chamber-ci.service; then
    echo "CI is running. Use its self-update endpoint, or stop it before reinstalling." >&2
    exit 1
fi

umask 077
config_dir=$HOME/.config/chamber-ci
service_dir=$HOME/.config/systemd/user
mkdir -p "$config_dir" "$service_dir/chamber-ci.service.d" "$HOME/.local/bin"
staging=$(mktemp -d "$HOME/.local/bin/.chamber-ci.XXXXXX")
trap 'rm -rf -- "$staging"' EXIT
# Use the installed toolchain; an older Go must not download a replacement.
GOTOOLCHAIN=local go -C "$repo_dir" build -o "$staging/chamber-ci" ./cmd/ci
install -m 0755 "$staging/chamber-ci" "$HOME/.local/bin/chamber-ci"

if [[ ! -e $config_dir/config.json ]]; then
    # Reuse the application's canonical example, substituting only owned paths.
    # Escape sed replacement characters in home paths.
    escaped_home=$(printf '%s' "$HOME" | sed 's/[\\&|]/\\&/g')
    sed -e "s|/home/you|$escaped_home|" \
        -e "s|\"root\":|\"work_root\": \"$escaped_home/ci-work\",\\n  \"root\":|" \
        "$repo_dir/cmd/ci/config.example.json" > "$config_dir/config.json"
fi
if [[ ! -e $config_dir/secrets.env ]]; then
    webhook_secret=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
    printf 'CHAMBER_GITHUB_TOKEN=\nCHAMBER_WEBHOOK_SECRET=%s\n' "$webhook_secret" > "$config_dir/secrets.env"
fi
chmod 0600 "$config_dir/config.json" "$config_dir/secrets.env"
install -m 0600 "$repo_dir/cmd/ci/ci.service" "$service_dir/chamber-ci.service"
cat > "$service_dir/chamber-ci.service.d/go.conf" <<EOF
[Service]
Environment="PATH=$go_bin_dir:/usr/local/bin:/usr/bin:/bin"
EOF
systemctl --user daemon-reload
cat <<EOF
Installed. Existing config and secrets were preserved.
Edit $config_dir/config.json (public_url, repositories, trusted update source).
Edit $config_dir/secrets.env (GitHub token with commit-status write permission).
Then run:
  XDG_RUNTIME_DIR=/run/user/$UID systemctl --user enable --now chamber-ci
  curl --fail http://127.0.0.1:8080/health
  XDG_RUNTIME_DIR=/run/user/$UID journalctl --user -u chamber-ci -f
Expose the loopback listener through HTTPS; see cmd/ci/README.md for webhooks.
EOF
