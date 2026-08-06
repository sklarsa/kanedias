#!/usr/bin/env bash
# This script starts a temporary Incus instance, then invokes itself there with
# --inside-instance to sync repositories on the persistent workspace volume.
set -Eeuo pipefail

sync_repositories() {
    local repos_file=$1
    local repos_dir=$2
    local url name target target_root actual_worktree default_ref branch
    local -A destinations=()

    if [[ -L $repos_dir ]]; then
        echo "refusing symlinked repository root: $repos_dir" >&2
        return 1
    fi
    if [[ -e $repos_dir && ! -d $repos_dir ]]; then
        echo "repository root is not a directory: $repos_dir" >&2
        return 1
    fi

    while IFS= read -r url || [[ -n $url ]]; do
        [[ -n $url ]] || continue
        name=${url##*/}
        name=${name%.git}
        if [[ -z $name || $name == . || $name == .. ]]; then
            echo "cannot derive repository name from: $url" >&2
            return 1
        fi
        if [[ -n ${destinations[$name]+present} ]]; then
            echo "duplicate repository destination: $name" >&2
            return 1
        fi
        destinations[$name]=$url
    done < "$repos_file"

    mkdir -p "$repos_dir"

    while IFS= read -r url || [[ -n $url ]]; do
        [[ -n $url ]] || continue

        name=${url##*/}
        name=${name%.git}
        target="$repos_dir/$name"

        printf 'Syncing %s...\n' "$url"
        if [[ -L $target ]]; then
            echo "refusing symlinked repository path: $target" >&2
            return 1
        elif [[ ! -e $target ]]; then
            git clone --recurse-submodules "$url" "$target"
        elif [[ ! -d $target/.git || -L $target/.git ]]; then
            echo "existing path is not a self-contained Git repository: $target" >&2
            return 1
        fi

        target_root=$(cd -- "$target" && pwd -P)
        if ! actual_worktree=$(git -C "$target" rev-parse --show-toplevel) ||
            ! actual_worktree=$(cd -- "$actual_worktree" && pwd -P) ||
            [[ $actual_worktree != "$target_root" ]]; then
            echo "repository worktree escapes its expected path: $target" >&2
            return 1
        fi

        git -C "$target" remote set-url origin "$url"
        git -C "$target" fetch --force --prune --prune-tags --tags origin
        git -C "$target" remote set-head origin --auto

        default_ref=$(git -C "$target" symbolic-ref refs/remotes/origin/HEAD)
        branch=${default_ref#refs/remotes/origin/}
        git -C "$target" checkout --force -B "$branch" "$default_ref"
        git -C "$target" reset --hard "$default_ref"
        git -C "$target" clean -ffdx
        git -C "$target" submodule sync --recursive
        git -C "$target" submodule update --init --recursive --force
        git -C "$target" submodule foreach --recursive \
            'git reset --hard && git clean -ffdx'
    done < "$repos_file"
}

if [[ ${1:-} == --inside-instance ]]; then
    if (( $# != 3 )); then
        echo "usage: $0 --inside-instance <repos-file> <repos-dir>" >&2
        exit 2
    fi
    sync_repositories "$2" "$3"
    exit
fi

if (( $# != 1 )); then
    echo "usage: $0 <image-alias>" >&2
    exit 2
fi

command -v incus >/dev/null 2>&1 || {
    echo "incus is required" >&2
    exit 1
}

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
script_path="$script_dir/$(basename -- "${BASH_SOURCE[0]}")"
repos_file=${INCUS_REPOS_FILE:-"$script_dir/private/repos.txt"}
image=$1
pool=${INCUS_STORAGE_POOL:-default}
volume=${INCUS_WORKSPACE_VOLUME:-agent-workspace-seed}
managed_user=${INCUS_WORKSPACE_USER:-kanedias}
managed_home="/home/$managed_user"
workspace_path=/workspace
instance="workspace-sync-$(date +%s)-$$"
instance_created=0
workspace_attached=0

cleanup() {
    local failed=0

    if (( instance_created )); then
        printf 'Stopping temporary instance %s...\n' "$instance"
        if ! incus stop --force "$instance" >/dev/null 2>&1; then
            echo "failed to stop temporary instance: $instance" >&2
            failed=1
        fi

        if (( workspace_attached )); then
            printf 'Detaching workspace volume...\n'
            if ! incus config device remove "$instance" workspace \
                >/dev/null 2>&1; then
                echo "failed to detach workspace volume from: $instance" >&2
                failed=1
            fi
        fi

        printf 'Deleting temporary instance %s...\n' "$instance"
        if incus delete --force "$instance" >/dev/null 2>&1; then
            instance_created=0
            workspace_attached=0
            ssh_attached=0
        else
            echo "failed to delete temporary instance: $instance" >&2
            failed=1
        fi
    fi

    (( failed == 0 ))
}

on_exit() {
    local status=$?
    trap - EXIT
    if ! cleanup && (( status == 0 )); then
        status=1
    fi
    exit "$status"
}
trap on_exit EXIT

[[ -s $repos_file ]] || {
    echo "missing or empty repository list: $repos_file" >&2
    exit 1
}

if incus storage volume show "$pool" "$volume" >/dev/null 2>&1; then
    printf 'Using existing workspace volume %s/%s.\n' "$pool" "$volume"
else
    printf 'Creating workspace volume %s/%s...\n' "$pool" "$volume"
    incus storage volume create "$pool" "$volume"
fi

printf 'Creating temporary instance %s from %s...\n' "$instance" "$image"
incus init "$image" "$instance"
instance_created=1

printf 'Attaching workspace volume...\n'
incus config device add "$instance" workspace disk \
    pool="$pool" source="$volume" path="$workspace_path"
workspace_attached=1

printf 'Starting temporary instance %s...\n' "$instance"
incus start "$instance"
incus exec "$instance" -- chown "$managed_user:$managed_user" "$workspace_path"
incus exec "$instance" -- install -d -o "$managed_user" -g "$managed_user" \
    "$workspace_path/repos"
incus file push "$repos_file" "$instance$managed_home/repos.txt"
incus file push "$script_path" "$instance$managed_home/sync-workspace.sh"

printf 'Synchronizing repositories...\n'
incus exec "$instance" -- runuser -u "$managed_user" -- \
    env HOME="$managed_home" USER="$managed_user" LOGNAME="$managed_user" \
    bash "$managed_home/sync-workspace.sh" --inside-instance \
    "$managed_home/repos.txt" "$workspace_path/repos"

if ! cleanup; then
    echo "workspace synchronized, but temporary resource cleanup failed" >&2
    exit 1
fi
trap - EXIT
printf 'Workspace volume %s/%s synchronized successfully.\n' "$pool" "$volume"
