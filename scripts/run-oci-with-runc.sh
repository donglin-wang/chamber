#!/usr/bin/env bash
set -euo pipefail

# Toy pipeline:
#   registry image -> local OCI image layout -> OCI runtime bundle -> runc
#
# Dependencies on Linux:
#   skopeo: pulls/copies the image into an OCI layout
#   umoci: unpacks the OCI image layout into a rootless runc bundle
#   python3: patches the OCI config with the current user's UID/GID map
#   runc: runs the bundle without sudo

usage() {
  cat <<'EOF'
Usage:
  scripts/run-oci-with-runc.sh [IMAGE]

Examples:
  scripts/run-oci-with-runc.sh
  scripts/run-oci-with-runc.sh docker.io/library/busybox:latest

Notes:
  - Defaults to docker.io/library/alpine:latest.
  - This is intended for Linux. On macOS, run it inside a Linux VM.
  - Run this as a normal user. It intentionally refuses to run as root.
  - The container's root user is mapped to your host UID/GID.
  - Uses the host network/cgroup namespaces to keep the toy rootless setup small.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  cat >&2 <<'EOF'
This script needs Linux because runc creates Linux namespaces and cgroups.
Run it inside a Linux VM or remote Linux host.
EOF
  exit 1
fi

if [[ "$EUID" -eq 0 ]]; then
  cat >&2 <<'EOF'
Refusing to run as root.
This example is meant to demonstrate rootless runc: run it as a normal user.
EOF
  exit 1
fi

if [[ -r /proc/sys/kernel/unprivileged_userns_clone ]] &&
  [[ "$(cat /proc/sys/kernel/unprivileged_userns_clone)" != "1" ]]; then
  cat >&2 <<'EOF'
Unprivileged user namespaces are disabled on this Linux host.

Try, inside the Linux VM:
  sudo sysctl -w kernel.unprivileged_userns_clone=1
EOF
  exit 1
fi

if [[ -r /proc/sys/user/max_user_namespaces ]] &&
  [[ "$(cat /proc/sys/user/max_user_namespaces)" == "0" ]]; then
  cat >&2 <<'EOF'
This Linux host allows zero user namespaces.

Try, inside the Linux VM:
  sudo sysctl -w user.max_user_namespaces=28633
EOF
  exit 1
fi

if [[ -r /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]] &&
  [[ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns)" == "1" ]]; then
  cat >&2 <<'EOF'
AppArmor is restricting unprivileged user namespaces on this host.

For a disposable local VM, the quick fix is:
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0

For a shared or hardened machine, prefer a proper AppArmor policy instead.
EOF
  exit 1
fi

missing=()
for tool in skopeo umoci runc python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    missing+=("$tool")
  fi
done

if ((${#missing[@]})); then
  printf 'Missing required tools: %s\n\n' "${missing[*]}" >&2
  cat >&2 <<'EOF'
Install hints:
  Debian/Ubuntu: sudo apt-get install -y skopeo umoci runc python3
  Fedora:        sudo dnf install -y skopeo umoci runc python3
  Arch:          sudo pacman -S skopeo umoci runc python
EOF
  exit 1
fi

if command -v unshare >/dev/null 2>&1 &&
  ! unshare --user --map-root-user true >/dev/null 2>&1; then
  cat >&2 <<'EOF'
Could not create a simple rootless user namespace with:
  unshare --user --map-root-user true

The Linux VM is blocking unprivileged user namespaces. Check the sysctl
settings above, or use a VM/kernel that enables rootless containers.
EOF
  exit 1
fi

image="${1:-docker.io/library/alpine:latest}"
tag="pulled"
container_id="toy-runc-$$"
workdir="$(mktemp -d)"
oci_layout="$workdir/image"
bundle="$workdir/bundle"
runc_root="$workdir/runc-state"

cleanup() {
  if [[ "${started_container:-}" == "yes" ]]; then
    runc --root "$runc_root" delete -f "$container_id" >/dev/null 2>&1 || true
  fi
  rm -rf "$workdir"
}
trap cleanup EXIT

echo "Pulling $image into an OCI image layout..."
skopeo copy "docker://$image" "oci:$oci_layout:$tag"

echo "Unpacking OCI image layout into a rootless OCI runtime bundle..."
umoci unpack --rootless --image "$oci_layout:$tag" "$bundle"

echo "Patching bundle config for a rootless runc user namespace..."
python3 - "$bundle/config.json" "$(id -u)" "$(id -g)" <<'PY'
import json
import sys

config_path = sys.argv[1]
host_uid = int(sys.argv[2])
host_gid = int(sys.argv[3])

with open(config_path, "r", encoding="utf-8") as f:
    config = json.load(f)

linux = config.setdefault("linux", {})
namespaces = linux.setdefault("namespaces", [])

if not any(namespace.get("type") == "user" for namespace in namespaces):
    namespaces.append({"type": "user"})

# Network and cgroup namespaces often need extra rootless networking or cgroup
# delegation setup. Reuse the host namespaces for this small example.
linux["namespaces"] = [
    namespace for namespace in namespaces
    if namespace.get("type") not in {"network", "cgroup"}
]

linux["uidMappings"] = [{"containerID": 0, "hostID": host_uid, "size": 1}]
linux["gidMappings"] = [{"containerID": 0, "hostID": host_gid, "size": 1}]

# Keep this toy example independent of cgroup delegation setup.
config.pop("cgroupsPath", None)
linux.pop("resources", None)
config["mounts"] = [
    mount for mount in config.get("mounts", [])
    if mount.get("type") != "cgroup" and mount.get("destination") != "/sys/fs/cgroup"
]

with open(config_path, "w", encoding="utf-8") as f:
    json.dump(config, f, indent=2)
    f.write("\n")
PY

cat <<EOF

Bundle created at:
  $bundle

Rootless mapping:
  container uid 0 -> host uid $(id -u)
  container gid 0 -> host gid $(id -g)

Running it with rootless runc now. The image's default command will execute.
EOF

cd "$bundle"
started_container=yes
runc --root "$runc_root" run "$container_id"
