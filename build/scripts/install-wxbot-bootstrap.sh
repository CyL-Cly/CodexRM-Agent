#!/bin/sh
# Provision the fixed system service for a Linux Agent installed from a trusted bundle.
set -eu
if [ "$(id -u)" -ne 0 ] || [ "$#" -ne 2 ]; then
    echo 'Usage: sudo sh install-wxbot-bootstrap.sh AGENT_BINARY AGENT_USER' >&2
    exit 2
fi
agent_binary=$1
agent_user=$2
case "$agent_user" in ''|*[!a-zA-Z0-9_-]*) exit 2 ;; esac
id -u "$agent_user" >/dev/null
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
unit="$script_dir/codexrm-wx-docker-bootstrap.service"
if [ ! -f "$unit" ]; then
    unit="$script_dir/../../deploy/codexrm-wx-docker-bootstrap.service"
fi
install -d -o root -g root -m 0755 /usr/libexec
install -o root -g root -m 0755 -- "$agent_binary" /usr/libexec/codexrm-wx-bootstrap
install -o root -g root -m 0644 -- "$unit" /etc/systemd/system/codexrm-wx-docker-bootstrap.service
systemctl daemon-reload
if command -v visudo >/dev/null 2>&1; then
    template=$(mktemp)
    trap 'rm -f -- "$template"' EXIT HUP INT TERM
    printf '%s ALL=(root) NOPASSWD: /usr/bin/systemctl start codexrm-wx-docker-bootstrap.service\n' "$agent_user" > "$template"
    visudo -cf "$template"
    install -o root -g root -m 0440 "$template" /etc/sudoers.d/codexrm-wx-docker-bootstrap
fi
