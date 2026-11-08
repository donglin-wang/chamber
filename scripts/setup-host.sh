#!/usr/bin/env bash
# Recovered from c18f4d1 and scoped to SDK host preparation.
set -euo pipefail

allow_unrestricted_userns=false
if [[ ${1:-} == --allow-unrestricted-userns ]]; then
    allow_unrestricted_userns=true
    shift
fi
if [[ ${1:-} == --help ]]; then
    echo "Usage: sudo $0 [--allow-unrestricted-userns] [ACCOUNT]"
    echo "Creates an unprivileged account (default: chamber) with one million subordinate IDs."
    echo "The optional flag permits disabling Ubuntu's system-wide AppArmor userns restrictions."
    exit 0
fi
[[ $# -le 1 ]] || { echo "Unexpected arguments; use --help." >&2; exit 2; }
[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo "Run as root on Linux." >&2; exit 1; }
account=${1:-chamber}
[[ $account =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo "Invalid account name." >&2; exit 2; }
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. /etc/os-release
case $ID in
    debian) . "$script_dir/distros/debian.sh" ;;
    ubuntu) . "$script_dir/distros/ubuntu.sh" ;;
    fedora|rocky|almalinux) . "$script_dir/distros/redhat.sh" ;;
    *) echo "Unsupported distribution: $ID" >&2; exit 1 ;;
esac

restricted=()
for setting in kernel.apparmor_restrict_unprivileged_userns kernel.apparmor_restrict_unprivileged_unconfined; do
    if [[ -r /proc/sys/${setting//./\/} ]] && [[ $(cat "/proc/sys/${setting//./\/}") != 0 ]]; then
        restricted+=("$setting")
    fi
done
if (( ${#restricted[@]} )) && ! $allow_unrestricted_userns; then
    echo "AppArmor restricts unprivileged user namespaces. Review the host policy, then rerun with --allow-unrestricted-userns to permit a system-wide change." >&2
    exit 1
fi
install_host_packages
for flag in --map-users --map-groups; do
    unshare --help | grep -q -- "$flag" || {
        echo "Chamber requires unshare with $flag (util-linux 2.38+). Use a newer distro release or install a current util-linux before setup." >&2
        exit 1
    }
done
for file in /etc/subuid /etc/subgid; do
    [[ -e $file ]] || install -m 0644 /dev/null "$file"
done
if ! id "$account" >/dev/null 2>&1; then
    useradd --create-home --user-group --shell /bin/bash \
        -K SUB_UID_COUNT=1000000 -K SUB_GID_COUNT=1000000 "$account"
fi
account_uid=$(id -u "$account")
[[ $account_uid != 0 ]] || { echo "Choose a non-root account." >&2; exit 1; }
for file in /etc/subuid /etc/subgid; do
    awk -F: -v name="$account" -v uid="$account_uid" '
        ($1 == name || $1 == uid) && !seen++ { valid = $3 >= 1000000 }
        END { exit !valid }
    ' "$file" || { echo "$account needs a first range of at least one million IDs in $file; choose a fresh account. Existing allocations were preserved." >&2; exit 1; }
done
install -d -m 0755 /etc/sysctl.d
echo 'user.max_user_namespaces = 28633' > /etc/sysctl.d/99-chamber.conf
if [[ -e /proc/sys/kernel/unprivileged_userns_clone ]]; then
    echo 'kernel.unprivileged_userns_clone = 1' >> /etc/sysctl.d/99-chamber.conf
fi
if $allow_unrestricted_userns; then
    for setting in kernel.apparmor_restrict_unprivileged_userns kernel.apparmor_restrict_unprivileged_unconfined; do
        if [[ -e /proc/sys/${setting//./\/} ]]; then
            echo "$setting = 0" >> /etc/sysctl.d/99-chamber.conf
        fi
    done
fi
sysctl -p /etc/sysctl.d/99-chamber.conf
runuser -u "$account" -- unshare -Ur true
echo "Host ready for $account. Go must be installed separately; SDK operations run without sudo."
