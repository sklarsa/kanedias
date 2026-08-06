#!/usr/bin/env bash
set -Eeuo pipefail

if (( $# != 1 )); then
    echo "usage: $0 <image-alias>" >&2
    exit 2
fi

image_alias=$1
source_image=${INCUS_BUILD_SOURCE:-images:debian/13}
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
assets_dir="$script_dir/assets"
private_dir="$script_dir/private"
container="image-build-$(date +%s)-$$"
installer="$script_dir/install.sh"
profile="$container-profile"
profile_file="$script_dir/profiles/image-build.yaml"
asset_inputs=(
    "$private_dir/authorized_hosts"
    "$assets_dir/pi-settings.json"
    "$assets_dir/cobalt-ember.json"
    "$assets_dir/tmux.conf"
)

cleanup() {
    incus delete --force "$container" >/dev/null 2>&1 || true
    incus profile delete "$profile" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v incus >/dev/null 2>&1 || {
    echo "incus is required" >&2
    exit 1
}

[[ -f $profile_file ]] || {
    echo "missing Incus profile: $profile_file" >&2
    exit 1
}

for input in "$installer" "${asset_inputs[@]}"; do
    if [[ ! -f $input ]]; then
        echo "missing build input: $input" >&2
        exit 1
    fi
done

printf 'Creating temporary profile %s...\n' "$profile"
incus profile create "$profile"
incus profile edit "$profile" < "$profile_file"

printf 'Launching temporary container %s from %s...\n' \
    "$container" "$source_image"
incus launch "$source_image" "$container" \
    --profile default --profile "$profile"

printf 'Copying build inputs...\n'
incus file push "$installer" "$container/root/install.sh"
incus exec "$container" -- install -d /root/assets
for input in "${asset_inputs[@]}"; do
    incus file push "$input" "$container/root/assets/$(basename "$input")"
done

printf 'Running build...\n'
incus exec "$container" -- bash /root/install.sh

printf 'Stopping %s...\n' "$container"
incus stop "$container"

printf 'Publishing image alias %s...\n' "$image_alias"
incus publish "$container" --alias "$image_alias" --reuse \
    description="kanedias sandbox from $source_image"

printf 'Published image %s from %s.\n' "$image_alias" "$source_image"
