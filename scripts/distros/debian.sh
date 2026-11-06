#!/usr/bin/env bash
# Sourced by setup-host.sh as root. Keep distro package/policy differences here.
install_host_packages() {
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y \
        ca-certificates curl git uidmap util-linux dbus-user-session
}
