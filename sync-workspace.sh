#!/usr/bin/env bash
# This script starts a temporary Incus instance, then invokes itself there with
# --inside-instance to sync repositories on the persistent workspace volume.
set -Eeuo pipefail

repository_https_url() {
    local slug=$1

    if [[ ! $slug =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]]; then
        printf 'invalid GitHub repository slug: %s\n' "$slug" >&2
        return 1
    fi
    printf 'https://github.com/%s.git\n' "$slug"
}

sync_repositories() {
    local repos_file=$1
    local repos_dir=$2
    local slug url name target target_root actual_worktree default_ref branch
    local -A destinations=()

    if [[ -L $repos_dir ]]; then
        echo "refusing symlinked repository root: $repos_dir" >&2
        return 1
    fi
    if [[ -e $repos_dir && ! -d $repos_dir ]]; then
        echo "repository root is not a directory: $repos_dir" >&2
        return 1
    fi

    while IFS= read -r slug || [[ -n $slug ]]; do
        [[ -n $slug ]] || continue
        if ! url=$(repository_https_url "$slug"); then
            return 1
        fi
        name=${slug##*/}
        if [[ -z $name || $name == . || $name == .. ]]; then
            echo "cannot derive repository name from: $slug" >&2
            return 1
        fi
        if [[ -n ${destinations[$name]+present} ]]; then
            echo "duplicate repository destination: $name" >&2
            return 1
        fi
        destinations[$name]=$url
    done < "$repos_file"

    gh auth setup-git --hostname github.com --force
    git config --global --replace-all url.https://github.com/.insteadOf \
        git@github.com:
    git config --global --add url.https://github.com/.insteadOf \
        ssh://git@github.com/
    mkdir -p "$repos_dir"

    while IFS= read -r slug || [[ -n $slug ]]; do
        [[ -n $slug ]] || continue

        name=${slug##*/}
        url=${destinations[$name]}
        target="$repos_dir/$name"

        printf 'Syncing %s...\n' "$slug"
        if [[ -L $target ]]; then
            echo "refusing symlinked repository path: $target" >&2
            return 1
        elif [[ ! -e $target ]]; then
            gh repo clone "$url" "$target" -- --recurse-submodules
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
command -v go >/dev/null 2>&1 || {
    echo "go is required" >&2
    exit 1
}
command -v timeout >/dev/null 2>&1 || {
    echo "timeout is required" >&2
    exit 1
}

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
script_path="$script_dir/$(basename -- "${BASH_SOURCE[0]}")"
profile_name=sandbox
profile_file="$script_dir/profiles/sandbox.yaml"
repos_file=${INCUS_REPOS_FILE:-"$script_dir/private/repos.txt"}
image=$1
pool=${INCUS_STORAGE_POOL:-default}
volume=${INCUS_WORKSPACE_VOLUME:-agent-workspace-seed}
managed_user=${INCUS_WORKSPACE_USER:-kanedias}
managed_home="/home/$managed_user"
workspace_path=/workspace
dns_timeout=${INCUS_DNS_TIMEOUT:-60}
if [[ ! $dns_timeout =~ ^[1-9][0-9]{0,3}$ ]] ||
    (( 10#$dns_timeout > 3600 )); then
    echo "INCUS_DNS_TIMEOUT must be an integer from 1 to 3600" >&2
    exit 2
fi
instance="workspace-sync-$(date +%s)-$$"
instance_created=0
workspace_attached=0

wait_for_dns() {
    local deadline=$((SECONDS + dns_timeout))
    local remaining

    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        if timeout "${remaining}s" incus exec "$instance" -- \
            getent ahosts github.com >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done

    printf 'Timed out waiting for DNS in %s.\n' "$instance" >&2
    return 1
}

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
[[ -f $profile_file ]] || {
    echo "missing Incus profile: $profile_file" >&2
    exit 1
}

if incus storage volume show "$pool" "$volume" >/dev/null 2>&1; then
    printf 'Using existing workspace volume %s/%s.\n' "$pool" "$volume"
else
    printf 'Creating workspace volume %s/%s...\n' "$pool" "$volume"
    incus storage volume create "$pool" "$volume"
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

printf 'Creating temporary instance %s from %s...\n' "$instance" "$image"
incus init "$image" "$instance" \
    --profile default --profile "$profile_name"
instance_created=1

printf 'Attaching workspace volume...\n'
incus config device override "$instance" workspace \
    pool="$pool" source="$volume" path="$workspace_path"
workspace_attached=1

printf 'Starting temporary instance %s...\n' "$instance"
incus start "$instance"
printf 'Waiting for DNS in %s...\n' "$instance"
wait_for_dns
printf 'Updating trusted CA certificates...\n'
incus exec "$instance" -- update-ca-certificates
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
