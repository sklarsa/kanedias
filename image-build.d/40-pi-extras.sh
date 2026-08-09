#!/usr/bin/env bash
set -Eeuo pipefail

if (( EUID != 0 )); then
    echo "$(basename "$0") must run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

managed_user=kanedias
managed_home=/home/kanedias

run_as_managed_user() (
    cd "$managed_home"
    exec runuser -u "$managed_user" -- \
        env HOME="$managed_home" USER="$managed_user" LOGNAME="$managed_user" \
        "$@"
)

run_as_managed_user env NVM_DIR="$managed_home/.nvm" bash <<'EOF'
set -Eeuo pipefail

# shellcheck source=/dev/null
source "$NVM_DIR/nvm.sh"
nvm use --silent default

pi_binary="$(dirname "$(command -v node)")/pi"
GIT_TERMINAL_PROMPT=0 "$pi_binary" install git:github.com/obra/superpowers
"$pi_binary" install npm:pi-web-suite
"$pi_binary" install npm:@diegopetrucci/pi-openai-fast
EOF

printf '%s\n' '{"enabled":true}' |
    install -D -m 0644 -o "$managed_user" -g "$managed_user" \
        /dev/stdin "$managed_home/.pi/agent/extensions/openai-fast.json"
