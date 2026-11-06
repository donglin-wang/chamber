#!/usr/bin/env bash
# Run through sudo; install-ci.sh runs separately as the service user.
set -euo pipefail

if [[ ${1:-} == --help ]]; then
    echo "Usage: sudo $0 [CI_USER]"
    echo "Creates a dedicated CI user (default: chamber-ci) with large subordinate ID ranges."
    exit 0
fi
[[ $# -le 1 ]] || { echo "Usage: sudo $0 [CI_USER]" >&2; exit 2; }
[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo "Run on Linux through sudo." >&2; exit 1; }
account=${1:-chamber-ci}

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. /etc/os-release
case $ID in
    debian) . "$script_dir/distros/debian.sh" ;;
    ubuntu|fedora|rhel|rocky|almalinux|arch)
        echo "$ID support is a skeleton; implement scripts/distros/$ID.sh first." >&2
        exit 1 ;;
    *) echo "Unsupported distribution: $ID" >&2; exit 1 ;;
esac

install_host_packages

# useradd allocates ranges only when these files exist.
for file in /etc/subuid /etc/subgid; do
    [[ -e $file ]] || install -m 0644 /dev/null "$file"
done
if ! id "$account" >/dev/null 2>&1; then
    useradd --create-home --user-group --shell /bin/bash \
        -K SUB_UID_COUNT=1000000 -K SUB_GID_COUNT=1000000 "$account"
fi
[[ $(id -u "$account") != 0 ]] || { echo "CI must belong to a non-root user." >&2; exit 1; }
# Reruns may reuse the account, but never rewrite existing allocations.
for kind in user group; do
    flags=()
    [[ $kind == user ]] || flags=(-g)
    getsubids "${flags[@]}" "$account" | awk 'NR == 1 && $4 >= 1000000 { valid = 1 } END { exit !valid }' || {
        echo "$account needs a large subordinate $kind range; choose a new CI username." >&2; exit 1;
    }
done
cat > /etc/sysctl.d/99-chamber.conf <<'EOF'
-kernel.unprivileged_userns_clone = 1
user.max_user_namespaces = 28633
EOF
sysctl -p /etc/sysctl.d/99-chamber.conf
loginctl enable-linger "$account"
echo "Host ready. Switch to the CI account with: sudo -iu $account"
