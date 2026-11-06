#!/usr/bin/env bash
# Copy to <os-release ID>.sh and add its explicit dispatch in setup-host.sh.
# Ubuntu, Fedora/RHEL, and Arch need their own package and LSM policy validation.
install_host_packages() {
    echo "Not implemented: install CA certificates, curl, Git, uidmap helpers," >&2
    echo "util-linux (unshare), and a systemd user session with D-Bus." >&2
    return 1
}
