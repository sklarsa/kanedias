#!/usr/bin/env bash
set -Eeuo pipefail

if (( $# < 1 || $# > 2 )); then
    echo "usage: $0 <image> [instance-name]" >&2
    exit 2
fi

image=$1
instance=${2:-sandbox}
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
profile_name=sandbox
profile_file="$script_dir/profiles/sandbox.yaml"
workspace_pool=default
workspace_seed=agent-workspace-seed
workspace_volume="agent-workspace-$instance"
instance_created=0
workspace_created=0

cleanup() {
    local status=$?
    trap - EXIT

    if (( status != 0 )); then
        if (( instance_created )); then
            printf 'Deleting failed sandbox %s...\n' "$instance" >&2
            if ! incus delete --force "$instance" >/dev/null 2>&1; then
                printf 'Failed to delete sandbox %s.\n' "$instance" >&2
            fi
        fi

        if (( workspace_created )); then
            printf 'Deleting failed workspace %s...\n' "$workspace_volume" >&2
            if ! incus storage volume delete "$workspace_pool" \
                "$workspace_volume" >/dev/null 2>&1; then
                printf 'Failed to delete workspace %s.\n' \
                    "$workspace_volume" >&2
            fi
        fi
    fi

    exit "$status"
}
trap cleanup EXIT

wait_for_systemd() {
    local deadline=$((SECONDS + 60))
    local remaining state

    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        state=$(timeout "${remaining}s" incus exec "$instance" -- \
            systemctl is-system-running --wait 2>/dev/null || true)
        case "$state" in
            running|degraded)
                return 0
                ;;
        esac
        sleep 1
    done

    printf 'Timed out waiting for systemd in %s.\n' "$instance" >&2
    return 1
}

command -v go >/dev/null 2>&1 || {
    echo "go is required" >&2
    exit 1
}
command -v incus >/dev/null 2>&1 || {
    echo "incus is required" >&2
    exit 1
}
command -v timeout >/dev/null 2>&1 || {
    echo "timeout is required" >&2
    exit 1
}
command -v flock >/dev/null 2>&1 || {
    echo "flock is required" >&2
    exit 1
}
[[ -f $profile_file ]] || {
    echo "missing Incus profile: $profile_file" >&2
    exit 1
}
if [[ $instance == */* || $instance == . || $instance == .. ]]; then
    printf 'Invalid instance name: %s\n' "$instance" >&2
    exit 2
fi
lock_root=${XDG_RUNTIME_DIR:-/tmp}
lock_dir=${KANEDIAS_SANDBOX_LOCK_DIR:-"$lock_root/kanedias-sandbox-locks-$UID"}
install -d -m 0700 "$lock_dir"
exec 9> "$lock_dir/$instance.lock"
if ! flock -n 9; then
    printf 'Another lifecycle operation is active for %s.\n' "$instance" >&2
    exit 1
fi

printf 'Initializing proxy CA...\n'
(
    cd "$script_dir"
    go run ./proxy -init-ca
)

if incus profile show "$profile_name" >/dev/null 2>&1; then
    printf 'Refreshing Incus profile %s...\n' "$profile_name"
else
    printf 'Creating Incus profile %s...\n' "$profile_name"
    incus profile create "$profile_name"
fi
incus profile edit "$profile_name" < "$profile_file"

printf 'Cloning workspace %s to %s...\n' \
    "$workspace_seed" "$workspace_volume"
incus storage volume copy \
    "$workspace_pool/$workspace_seed" \
    "$workspace_pool/$workspace_volume" \
    --volume-only
workspace_created=1

printf 'Initializing sandbox %s from %s...\n' "$instance" "$image"
incus init "$image" "$instance" \
    --profile default --profile "$profile_name"
instance_created=1

printf 'Attaching workspace %s to %s...\n' \
    "$workspace_volume" "$instance"
incus config device override "$instance" workspace \
    pool="$workspace_pool" source="$workspace_volume" path=/workspace

printf 'Starting sandbox %s...\n' "$instance"
incus start "$instance"

printf 'Waiting for systemd in %s...\n' "$instance"
wait_for_systemd

printf 'Updating trusted CA certificates in %s...\n' "$instance"
incus exec "$instance" -- update-ca-certificates

printf 'Sandbox %s is ready.\n' "$instance"
