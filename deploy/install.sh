#!/bin/sh
set -eu

usage() {
  cat >&2 <<'EOF'
Usage: sudo deploy/install.sh [--binary-path PATH]

Installs the service user, a default configuration, and the systemd unit for
mihomo-node-manager. Existing /etc/mihomo-node-manager files are never
overwritten; proposed replacements are written next to them with a .new
suffix.

Options:
  --binary-path PATH   render the unit to exec PATH directly instead of
                       installing a copy at /usr/local/bin/mihomo-node-manager
                       (e.g. the outputs/ binary of a source checkout). The
                       path must be absolute and executable.
EOF
  exit 1
}

if [ "$(id -u)" -ne 0 ]; then
  echo "Run this installer with sudo." >&2
  exit 1
fi

binary_target=/usr/local/bin/mihomo-node-manager
while [ $# -gt 0 ]; do
  case "$1" in
    --binary-path)
      [ $# -ge 2 ] || usage
      binary_target=$2
      shift 2
      ;;
    --binary-path=*)
      binary_target=${1#*=}
      shift
      ;;
    *)
      usage
      ;;
  esac
done
case "$binary_target" in
  /*) ;;
  *) echo "--binary-path must be an absolute path: $binary_target" >&2; exit 1 ;;
esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
unit_template="$script_dir/mihomo-node-manager.service.in"
config_source="$repo_dir/config/config.example.toml"
env_example_source="$repo_dir/.env.example"

for required in "$config_source" "$unit_template"; do
  if [ ! -f "$required" ]; then
    echo "Missing deployment file: $required" >&2
    exit 1
  fi
done

if [ "$binary_target" = "/usr/local/bin/mihomo-node-manager" ]; then
  binary_source="$repo_dir/outputs/mihomo-node-manager"
  if [ ! -f "$binary_source" ]; then
    echo "Missing $binary_source; build it first: make build" >&2
    exit 1
  fi
  install -m 0755 -o root -g root "$binary_source" "$binary_target"
  echo "Installed $binary_target"
elif [ ! -x "$binary_target" ]; then
  echo "Binary is missing or not executable: $binary_target" >&2
  exit 1
fi

# ProtectHome=true walls off /home, /root and /run/user. A binary kept there
# still has to be executable, so the unit is rendered with read-only instead.
case "$binary_target" in
  /home/*|/root/*) protect_home=read-only ;;
  *) protect_home=true ;;
esac

if ! getent group mihomo-node-manager >/dev/null 2>&1; then
  groupadd --system mihomo-node-manager
fi
if ! getent passwd mihomo-node-manager >/dev/null 2>&1; then
  useradd --system --gid mihomo-node-manager --home-dir /var/lib/mihomo-node-manager --no-create-home --shell /usr/sbin/nologin mihomo-node-manager
fi

install -d -m 0750 -o root -g mihomo-node-manager /etc/mihomo-node-manager
install -d -m 0750 -o mihomo-node-manager -g mihomo-node-manager /var/lib/mihomo-node-manager

if [ -e /etc/mihomo-node-manager/config.toml ]; then
  install -m 0640 -o root -g mihomo-node-manager "$config_source" /etc/mihomo-node-manager/config.toml.new
  echo "Kept existing config; proposed config is /etc/mihomo-node-manager/config.toml.new"
else
  install -m 0640 -o root -g mihomo-node-manager "$config_source" /etc/mihomo-node-manager/config.toml
fi

# Reference file for the CPA ping-pong credentials. The real .env is never
# created or overwritten by this script.
if [ -f "$env_example_source" ] && [ ! -e /etc/mihomo-node-manager/.env.example ]; then
  install -m 0640 -o root -g mihomo-node-manager "$env_example_source" /etc/mihomo-node-manager/.env.example
  echo "Installed /etc/mihomo-node-manager/.env.example"
fi
if [ ! -e /etc/mihomo-node-manager/.env ]; then
  echo "Note: no /etc/mihomo-node-manager/.env yet; the Gemini ping-pong probe stays disabled until you create it (see .env.example)."
fi

rendered_unit=$(mktemp)
trap 'rm -f "$rendered_unit"' EXIT
sed -e "s|@BINARY_PATH@|$binary_target|g" -e "s|@PROTECT_HOME@|$protect_home|g" "$unit_template" > "$rendered_unit"
install -m 0644 -o root -g root "$rendered_unit" /etc/systemd/system/mihomo-node-manager.service
echo "Rendered /etc/systemd/system/mihomo-node-manager.service (binary=$binary_target, ProtectHome=$protect_home)"

# Validate from the unit's working directory so pingpong.env_file resolves
# against the real /etc/mihomo-node-manager/.env as well.
cd /etc/mihomo-node-manager
"$binary_target" --config /etc/mihomo-node-manager/config.toml --check-config
systemctl daemon-reload
systemctl enable --now mihomo-node-manager.service
systemctl --no-pager --full status mihomo-node-manager.service | head -n 8
