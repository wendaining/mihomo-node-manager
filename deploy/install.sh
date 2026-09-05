#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "Run this installer with sudo." >&2
  exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary_source="$script_dir/mihomo-node-manager"
config_source="$script_dir/config.toml"
unit_source="$script_dir/mihomo-node-manager.service"

for required in "$binary_source" "$config_source" "$unit_source"; do
  if [ ! -f "$required" ]; then
    echo "Missing deployment file: $required" >&2
    exit 1
  fi
done

if ! getent group mihomo-node-manager >/dev/null 2>&1; then
  groupadd --system mihomo-node-manager
fi
if ! getent passwd mihomo-node-manager >/dev/null 2>&1; then
  useradd --system --gid mihomo-node-manager --home-dir /var/lib/mihomo-node-manager --no-create-home --shell /usr/sbin/nologin mihomo-node-manager
fi

install -m 0755 -o root -g root "$binary_source" /usr/local/bin/mihomo-node-manager
install -d -m 0750 -o root -g mihomo-node-manager /etc/mihomo-node-manager
install -d -m 0750 -o mihomo-node-manager -g mihomo-node-manager /var/lib/mihomo-node-manager

if [ -e /etc/mihomo-node-manager/config.toml ]; then
  install -m 0640 -o root -g mihomo-node-manager "$config_source" /etc/mihomo-node-manager/config.toml.new
  echo "Kept existing config; proposed config is /etc/mihomo-node-manager/config.toml.new"
else
  install -m 0640 -o root -g mihomo-node-manager "$config_source" /etc/mihomo-node-manager/config.toml
fi

install -m 0644 -o root -g root "$unit_source" /etc/systemd/system/mihomo-node-manager.service
/usr/local/bin/mihomo-node-manager --config /etc/mihomo-node-manager/config.toml --check-config
systemctl daemon-reload
systemctl enable --now mihomo-node-manager.service
systemctl --no-pager --full status mihomo-node-manager.service
