#!/usr/bin/env bash
# Run from the checkout root on a disposable SourceHut Linux VM.
set -euo pipefail

sudo ./scripts/setup-host.sh --allow-unrestricted-userns chamber-test

sudo install -d -o chamber-test -g chamber-test /home/chamber-test/source
tar --exclude=.git -cf - . |
    sudo -u chamber-test tar -C /home/chamber-test/source -xf -

sudo -iu chamber-test bash -s <<'TEST'
set -euo pipefail
export PATH=/usr/local/bin:/usr/bin:/bin
export GOTOOLCHAIN=auto
# Nested mapping helpers need file capabilities; /tmp may be mounted nosuid.
export TMPDIR="$HOME/tmp"
mkdir -p "$TMPDIR"
cd "$HOME/source"

go run ./cmd/local-test
TEST
