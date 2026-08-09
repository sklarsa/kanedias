#!/usr/bin/env bash
set -Eeuo pipefail

if (( EUID != 0 )); then
    echo "$(basename "$0") must run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

managed_user=kanedias
managed_home=/home/kanedias

printf '%s\n' \
    'set -g mouse on' \
    'set -g extended-keys on' |
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        /dev/stdin "$managed_home/.tmux.conf"
