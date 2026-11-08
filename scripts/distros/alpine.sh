#!/usr/bin/env bash
# Use shadow account tools and util-linux namespaces rather than BusyBox variants.
install_host_packages() {
    apk add --no-cache bash ca-certificates coreutils curl git gawk \
        shadow shadow-subids util-linux
}
