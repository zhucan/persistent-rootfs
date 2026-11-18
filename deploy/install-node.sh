#!/usr/bin/env sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "install-node.sh must run as root" >&2
  exit 1
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BINARY=${1:-"$SCRIPT_DIR/containerd-shim-persistent-v2"}

if [ ! -x "$BINARY" ]; then
  echo "runtime binary is missing or not executable: $BINARY" >&2
  exit 1
fi
if [ ! -x /usr/bin/runc ]; then
  echo "real runc was not found at /usr/bin/runc; update config.json first" >&2
  exit 1
fi

install -D -m 0755 "$BINARY" /usr/local/bin/containerd-shim-persistent-v2
install -D -m 0600 "$SCRIPT_DIR/config.json" /etc/persistent-rootfs/config.json
install -d -m 0700 /run/persistent-rootfs

echo "installed /usr/local/bin/containerd-shim-persistent-v2"
echo "merge the matching containerd TOML snippet, restart containerd, then label the node:"
echo "  kubectl label node <node> persistent-rootfs.io/enabled=true"
