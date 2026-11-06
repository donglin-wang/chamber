#!/usr/bin/env bash
# Run through sudo; install-ci.sh runs separately as the service user.
set -euo pipefail

if [[ ${1:-} == --help ]]; then
    echo "Usage: sudo $0 USER [SUBID_START]"
    echo "Allocates 1,000,000 subordinate UIDs/GIDs (default start: 1000000)."
    exit 0
fi
[[ $# -ge 1 && $# -le 2 ]] || { echo "Usage: sudo $0 USER [SUBID_START]" >&2; exit 2; }
[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo "Run on Linux through sudo." >&2; exit 1; }
account=$1
account_uid=$(id -u "$account")
[[ $account_uid != 0 ]] || { echo "CI must belong to a non-root user." >&2; exit 1; }
subid_start=${2:-1000000}
[[ $subid_start =~ ^[1-9][0-9]{0,9}$ ]] || { echo "Invalid subordinate ID start." >&2; exit 1; }
subid_end=$((subid_start + 999999))
(( subid_end < 4294967295 )) || { echo "Subordinate ID range exceeds Linux limits." >&2; exit 1; }

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. /etc/os-release
case $ID in
    debian) . "$script_dir/distros/debian.sh" ;;
    ubuntu|fedora|rhel|rocky|almalinux|arch)
        echo "$ID support is a skeleton; implement scripts/distros/$ID.sh first." >&2
        exit 1 ;;
    *) echo "Unsupported distribution: $ID" >&2; exit 1 ;;
esac

# Plan both mappings before changing either. Keep a sufficient first range.
mapping_options=()
for kind in uid gid; do
    file=/etc/sub${kind}
    [[ -e $file ]] || install -m 0644 /dev/null "$file"
    first_count=$(awk -F: -v name="$account" -v uid="$account_uid" '
        $1 == name || $1 == uid { print $3; exit }
    ' "$file")
    if [[ $first_count =~ ^[0-9]+$ ]] && (( first_count >= 1000000 )); then
        continue
    fi
    if ! awk -F: -v name="$account" -v uid="$account_uid" -v start="$subid_start" -v end="$subid_end" '
        $1 != name && $1 != uid && $2 <= end && $2 + $3 > start { exit 1 }
    ' "$file"; then
        echo "Requested range overlaps another allocation in $file; choose another SUBID_START." >&2
        exit 1
    fi
    database=passwd
    [[ $kind == uid ]] || database=group
    if ! getent "$database" | awk -F: -v start="$subid_start" -v end="$subid_end" '
        $3 >= start && $3 <= end { exit 1 }
    '; then
        echo "Requested range overlaps a host $kind; choose another SUBID_START." >&2
        exit 1
    fi
    while IFS=: read -r owner start count; do
        if [[ $owner == "$account" || $owner == "$account_uid" ]]; then
            mapping_options+=("--del-sub${kind}s" "$start-$((start + count - 1))")
        fi
    done < "$file"
    mapping_options+=("--add-sub${kind}s" "$subid_start-$subid_end")
done

install_host_packages
if ((${#mapping_options[@]})); then
    usermod "${mapping_options[@]}" "$account"
fi
cat > /etc/sysctl.d/99-chamber.conf <<'EOF'
-kernel.unprivileged_userns_clone = 1
user.max_user_namespaces = 28633
EOF
sysctl -p /etc/sysctl.d/99-chamber.conf
loginctl enable-linger "$account"
echo "Host ready for $account. Run scripts/install-ci.sh as that user in an SSH login."
