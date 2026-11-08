#!/usr/bin/env bash
# Fedora and Rocky/AlmaLinux provide the mapping helpers in shadow-utils.
install_host_packages() {
    dnf install -y ca-certificates curl git gawk shadow-utils util-linux
}
