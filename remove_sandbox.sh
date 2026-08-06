#!/usr/bin/env bash
set -Eeuo pipefail

if (( $# != 1 )) || [[ -z $1 ]]; then
    echo "usage: $0 <instance-name>" >&2
    exit 2
fi

instance=$1
workspace_pool=default
workspace_seed=agent-workspace-seed
workspace_volume="agent-workspace-$instance"

if [[ $workspace_volume == "$workspace_seed" ]]; then
    printf 'Refusing to remove protected workspace volume %s.\n' \
        "$workspace_seed" >&2
    exit 2
fi

command -v incus >/dev/null 2>&1 || {
    echo "incus is required" >&2
    exit 1
}
command -v flock >/dev/null 2>&1 || {
    echo "flock is required" >&2
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

instance_names=$(incus list --format csv -c n)
instance_exists=0
if grep -Fxq -- "$instance" <<< "$instance_names"; then
    instance_exists=1
fi

if (( instance_exists )); then
    if ! actual_workspace=$(incus config device get \
        "$instance" workspace source 2>/dev/null); then
        printf 'Refusing to remove %s: local workspace device is missing.\n' \
            "$instance" >&2
        exit 1
    fi
    if [[ $actual_workspace != "$workspace_volume" ]]; then
        printf 'Refusing to remove %s: workspace source is %s, expected %s.\n' \
            "$instance" "$actual_workspace" "$workspace_volume" >&2
        exit 1
    fi

    printf 'Deleting sandbox %s...\n' "$instance"
    incus delete --force "$instance"
fi

volume_records=$(incus storage volume list "$workspace_pool" \
    --format csv -c t,n)
volume_exists=0
if grep -Fxq -- "custom,$workspace_volume" <<< "$volume_records"; then
    volume_exists=1
fi

if (( volume_exists )); then
    printf 'Deleting workspace %s...\n' "$workspace_volume"
    incus storage volume delete "$workspace_pool" "$workspace_volume"
elif (( ! instance_exists )); then
    printf 'Sandbox %s and workspace %s are already absent.\n' \
        "$instance" "$workspace_volume"
fi
