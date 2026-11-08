#!/usr/bin/env bash
# Debian and Ubuntu package names are identical for these prerequisites.
install_host_packages() {
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y \
        ca-certificates curl git uidmap util-linux dbus-user-session
}
