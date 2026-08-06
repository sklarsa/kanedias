#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
assets_dir="$script_dir/assets"
installer="$script_dir/install.sh"
authorized_hosts_file=$(mktemp)
: > "$authorized_hosts_file"
profile_file="$script_dir/profiles/image-build.yaml"
pi_settings_file="$assets_dir/pi-settings.json"
pi_theme_file="$assets_dir/cobalt-ember.json"
tmux_config_file="$assets_dir/tmux.conf"
container="provision-test-$(date +%s)-$$"
profile="$container-profile"
image=${INCUS_TEST_IMAGE:-images:debian/13}

cleanup() {
    local status=$?

    rm -f "$authorized_hosts_file"
    if (( status != 0 )) && [[ ${KEEP_FAILED_CONTAINER:-0} == 1 ]]; then
        printf 'Keeping failed container %s and profile %s for debugging.\n' \
            "$container" "$profile" >&2
        return
    fi

    incus delete --force "$container" >/dev/null 2>&1 || true
    incus profile delete "$profile" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v incus >/dev/null 2>&1 || {
    echo "incus is required" >&2
    exit 1
}

[[ -f $installer ]] || {
    echo "missing installer: $installer" >&2
    exit 1
}

[[ -f $profile_file ]] || {
    echo "missing Incus profile: $profile_file" >&2
    exit 1
}

for required_file in \
    "$authorized_hosts_file" "$pi_settings_file" "$pi_theme_file" \
    "$tmux_config_file"; do
    if [[ ! -f $required_file ]]; then
        echo "missing test input: $required_file" >&2
        exit 1
    fi
done

printf 'Creating Incus profile %s...\n' "$profile"
incus profile create "$profile"
incus profile edit "$profile" < "$profile_file"

printf 'Launching %s from %s...\n' "$container" "$image"
incus launch "$image" "$container" --profile default --profile "$profile"

expanded_config=$(incus query "/1.0/instances/$container" | jq -c .expanded_config)
[[ $(jq -r '."security.nesting"' <<< "$expanded_config") == true ]]
[[ $(jq -r '."security.privileged"' <<< "$expanded_config") == false ]]
[[ $(jq -r '."raw.lxc" // ""' <<< "$expanded_config") == "" ]]
[[ $(jq -r '."security.syscalls.intercept.mknod" // ""' <<< "$expanded_config") == "" ]]
[[ $(jq -r '."security.syscalls.intercept.setxattr" // ""' <<< "$expanded_config") == "" ]]

printf 'Copying and running installer...\n'
incus file push "$installer" "$container/root/install.sh"
incus exec "$container" -- install -d /root/assets
incus file push "$authorized_hosts_file" "$container/root/assets/authorized_hosts"
incus file push "$pi_settings_file" "$container/root/assets/pi-settings.json"
incus file push "$pi_theme_file" "$container/root/assets/cobalt-ember.json"
incus file push "$tmux_config_file" "$container/root/assets/tmux.conf"
incus exec "$container" -- bash /root/install.sh

printf 'Verifying install...\n'
# The variables in this string expand in the container, not on the host.
# shellcheck disable=SC2016
incus exec "$container" -- bash -c '
    set -Eeuo pipefail
    trap "printf \"Verification failed at line %s: %s\\n\" \
        \"\$LINENO\" \"\$BASH_COMMAND\" >&2" ERR

    for package_name in \
        apt-transport-https bind9-dnsutils bsdutils ca-certificates clang \
        coreutils cpio curl dialog diffutils file findutils gcc gh git gnupg \
        grep groff gzip hostname htop iproute2 iputils-ping jq less locales lsb-release \
        make mawk net-tools nodejs openssh-client openssh-server procps python3 \
        sed shellcheck sudo tar time tmux tzdata unzip util-linux vim wget zip zsh \
        azure-cli google-cloud-cli google-cloud-cli-gke-gcloud-auth-plugin \
        session-manager-plugin containerd.io docker-buildx-plugin docker-ce \
        docker-ce-cli docker-compose-plugin podman claude-code; do
        dpkg-query -W "$package_name" >/dev/null
    done

    for command_name in \
        aws az clang claude curl dig docker file gcc gh git gcloud \
        gke-gcloud-auth-plugin go groff htop ip jq k9s kind less make node \
        podman pulumi python3 session-manager-plugin shellcheck ssh tmux unzip vim wget zip zsh; do
        command -v "$command_name" >/dev/null
    done

    [[ $(readlink -f "$(command -v go)") == /usr/local/go/bin/go ]]
    [[ $(command -v node) == /usr/bin/node ]]
    [[ $(command -v python3) == /usr/bin/python3 ]]
    [[ $(command -v aws) == /usr/local/bin/aws ]]

    [[ $(getent passwd kanedias | cut -d: -f6-7) == /home/kanedias:/usr/bin/zsh ]]
    id -nG kanedias | tr " " "\n" | grep -Fxq sudo
    id -nG kanedias | tr " " "\n" | grep -Fxq docker
    runuser -u kanedias -- sudo -n true

    cmp -s /root/assets/authorized_hosts /home/kanedias/.ssh/authorized_keys
    cmp -s /root/assets/tmux.conf /home/kanedias/.tmux.conf
    [[ $(stat -c "%U:%G:%a" /home/kanedias/.ssh) == kanedias:kanedias:700 ]]
    [[ $(stat -c "%U:%G:%a" /home/kanedias/.ssh/authorized_keys) == kanedias:kanedias:600 ]]
    [[ $(stat -c "%U:%G:%a" /home/kanedias/.tmux.conf) == kanedias:kanedias:644 ]]
    [[ $(stat -c "%U:%G:%a" /home/kanedias/.zshenv) == kanedias:kanedias:644 ]]
    [[ $(stat -c "%U:%G:%a" /home/kanedias/.zshrc) == kanedias:kanedias:644 ]]
    systemctl is-active --quiet ssh

    latest_go=$(curl -fsSL "https://go.dev/dl/?mode=json" | jq -er ".[0].version")
    latest_pulumi=$(curl -fsSL https://www.pulumi.com/latest-version)
    [[ $(go env GOVERSION) == "$latest_go" ]]
    [[ $(pulumi version) == "v$latest_pulumi" ]]
    [[ $(stat -c "%U:%G" /usr/local/bin/pulumi) == root:root ]]

    find /var/lib/apt/lists -maxdepth 1 -type f -name "*Packages*" -print -quit | grep -q .
    grep -Fq "Suites: bookworm" /etc/apt/sources.list.d/azure-cli.sources
    grep -Fq "https://packages.cloud.google.com/apt cloud-sdk main" /etc/apt/sources.list.d/google-cloud-sdk.list
    grep -Fq "https://download.docker.com/linux/debian" /etc/apt/sources.list.d/docker.sources
    grep -Fq "downloads.claude.ai/claude-code/apt/latest" /etc/apt/sources.list.d/claude-code.list
    grep -Fq "KubeletInUserNamespace: true" /etc/kind/config.yaml

    aws --version >/dev/null 2>&1
    az version --output json >/dev/null
    gcloud version >/dev/null
    gke-gcloud-auth-plugin --version >/dev/null
    session-manager-plugin --version >/dev/null
    docker compose version >/dev/null
    podman --version >/dev/null
    pulumi version >/dev/null
    k9s version >/dev/null
    kind version >/dev/null
    claude --version >/dev/null

    kanedias_env=(runuser -u kanedias -- env HOME=/home/kanedias USER=kanedias LOGNAME=kanedias)
    kanedias_node=$("${kanedias_env[@]}" zsh -ic "command -v node")
    kanedias_pi=$("${kanedias_env[@]}" zsh -ic "command -v pi")
    kanedias_pulumi=$("${kanedias_env[@]}" zsh -ic "command -v pulumi")
    kanedias_terraform=$("${kanedias_env[@]}" zsh -ic "command -v terraform")
    kanedias_tfenv=$("${kanedias_env[@]}" zsh -ic "command -v tfenv")
    [[ $kanedias_node == /home/kanedias/.nvm/versions/node/v22.*/bin/node ]]
    [[ $kanedias_pi == /home/kanedias/.nvm/versions/node/v22.*/bin/pi ]]
    [[ $kanedias_pulumi == /usr/local/bin/pulumi ]]
    [[ $kanedias_terraform == /home/kanedias/.local/bin/terraform ]]
    [[ $kanedias_tfenv == /home/kanedias/.local/bin/tfenv ]]
    [[ $("${kanedias_env[@]}" zsh -ic "command -v uv") == /home/kanedias/.local/bin/uv ]]
    "${kanedias_env[@]}" zsh -ic "node --version" | grep -Eq "^v22\\."
    "${kanedias_env[@]}" zsh -ic "nvm --version" >/dev/null
    "${kanedias_env[@]}" zsh -ic "npm --version" >/dev/null
    "${kanedias_env[@]}" zsh -ic "pi --version" >/dev/null
    "${kanedias_env[@]}" zsh -ic "tfenv --version" >/dev/null
    "${kanedias_env[@]}" zsh -ic "uv --version" >/dev/null

    [[ ! -e /home/kanedias/.tfenv/version ]]
    [[ ! -d /home/kanedias/.tfenv/versions ]] ||
        [[ -z $(find /home/kanedias/.tfenv/versions -mindepth 1 -maxdepth 1 \
            -type d -print -quit) ]]
    [[ ! -e /root/.nvm ]]
    [[ ! -e /root/.pi ]]
    [[ ! -e /usr/bin/terraform ]]
    [[ ! -e /usr/local/bin/pi ]]
    [[ ! -e /usr/local/bin/terraform ]]
    [[ ! -e /usr/local/bin/uv ]]
    [[ -z $(find /home/kanedias/.local /home/kanedias/.nvm /home/kanedias/.pi \
        /home/kanedias/.tfenv -not -user kanedias -print -quit) ]]

    cmp -s /root/assets/pi-settings.json /home/kanedias/.pi/agent/settings.json
    cmp -s /root/assets/cobalt-ember.json \
        /home/kanedias/.pi/agent/themes/cobalt-ember.json
    [[ $(stat -c "%U:%G:%a" /home/kanedias/.pi/agent/themes) == kanedias:kanedias:755 ]]
    [[ $(stat -c "%U:%G:%a" \
        /home/kanedias/.pi/agent/themes/cobalt-ember.json) == kanedias:kanedias:644 ]]
    [[ -d /home/kanedias/.pi/agent/git/github.com/obra/superpowers ]]
    [[ -d /home/kanedias/.pi/agent/npm/node_modules/pi-subagents ]]
    [[ -d /home/kanedias/.pi/agent/npm/node_modules/pi-web-suite ]]
    jq -e ".lastChangelogVersion == \"0.83.0\"
        and .theme == \"cobalt-ember\"
        and .defaultProvider == \"openai-codex\"
        and .defaultModel == \"gpt-5.6-sol\"
        and .defaultThinkingLevel == \"xhigh\"
        and .hideThinkingBlock == true
        and .packages == [
            \"git:github.com/obra/superpowers\",
            \"npm:pi-subagents\",
            \"npm:pi-web-suite\",
            \"npm:@diegopetrucci/pi-openai-fast\"
        ]" /home/kanedias/.pi/agent/settings.json >/dev/null
'

image_size=$(incus exec "$container" -- du -shx / | awk 'NR == 1 { print $1 }')
printf 'Installed image size: %s\n' "$image_size"
printf 'PASS: install completed successfully in %s\n' "$container"
