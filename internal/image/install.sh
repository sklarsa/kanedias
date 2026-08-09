#!/usr/bin/env bash
set -Eeuo pipefail

if (( EUID != 0 )); then
    echo "install.sh must run as root" >&2
    exit 1
fi

# shellcheck source=/dev/null
source /etc/os-release
if [[ ${ID:-} != debian || ${VERSION_ID:-} != 13 ]]; then
    echo "install.sh requires Debian 13" >&2
    exit 1
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
assets_dir="$script_dir/assets"
managed_user=kanedias
managed_home="/home/$managed_user"
authorized_hosts_file="$assets_dir/authorized_hosts"
pi_settings_file="$assets_dir/pi-settings.json"
pi_auth_file="$assets_dir/pi-auth.json"
pi_models_file="$assets_dir/pi-models.json"
pi_rpc_socket_file="$assets_dir/kanedias-pi.socket"
pi_rpc_service_file="$assets_dir/kanedias-pi@.service"
pi_environment_bridge_file="$assets_dir/kanedias-pi-env"
pi_rpc_launcher_file="$assets_dir/kanedias-pi-rpc"
pi_extension_dir="$assets_dir/pi-extension"

for required_file in \
    "$authorized_hosts_file" "$pi_settings_file" "$pi_auth_file" "$pi_models_file" \
    "$pi_rpc_socket_file" "$pi_rpc_service_file" \
    "$pi_environment_bridge_file" "$pi_rpc_launcher_file"; do
    if [[ ! -f $required_file ]]; then
        echo "missing install input: $required_file" >&2
        exit 1
    fi
done
if [[ ! -d $pi_extension_dir ]]; then
    echo "missing install input: $pi_extension_dir" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends \
    apt-transport-https \
    bind9-dnsutils \
    bsdutils \
    ca-certificates \
    coreutils \
    cpio \
    curl \
    dialog \
    diffutils \
    file \
    findutils \
    fd-find \
    gh \
    git \
    gnupg \
    grep \
    groff \
    gzip \
    hostname \
    htop \
    iproute2 \
    iputils-ping \
    jq \
    less \
    locales \
    lsb-release \
    make \
    mawk \
    net-tools \
    openssh-client \
    openssh-server \
    procps \
    python3 \
    ripgrep \
    sed \
    shellcheck \
    sudo \
    tar \
    time \
    tmux \
    tzdata \
    unzip \
    util-linux \
    vim \
    wget \
    zip \
    zsh

run_as_managed_user() (
    cd "$managed_home"
    exec runuser -u "$managed_user" -- \
        env HOME="$managed_home" USER="$managed_user" LOGNAME="$managed_user" \
        "$@"
)

configure_managed_user() {
    if id "$managed_user" >/dev/null 2>&1; then
        usermod --home "$managed_home" --shell /usr/bin/zsh "$managed_user"
    else
        useradd --create-home --shell /usr/bin/zsh "$managed_user"
    fi

    usermod --append --groups sudo "$managed_user"

    install -m 0440 /dev/null "/etc/sudoers.d/$managed_user"
    printf '%s ALL=(ALL:ALL) NOPASSWD: ALL\n' "$managed_user" \
        > "/etc/sudoers.d/$managed_user"
    visudo -cf "/etc/sudoers.d/$managed_user" >/dev/null

    install -d -m 0700 -o "$managed_user" -g "$managed_user" \
        "$managed_home/.ssh"
    install -m 0600 -o "$managed_user" -g "$managed_user" \
        "$authorized_hosts_file" "$managed_home/.ssh/authorized_keys"
    if [[ ! -e $managed_home/.zshrc ]]; then
        install -m 0644 -o "$managed_user" -g "$managed_user" \
            /dev/null "$managed_home/.zshrc"
    fi

    printf '%s\n' "export PATH=\"\$HOME/.local/bin:\$PATH\"" \
        > "$managed_home/.zshenv"
    chown "$managed_user:$managed_user" "$managed_home/.zshenv"
    chmod 0644 "$managed_home/.zshenv"

    # Configure git to use the GitHub CLI as its HTTPS credential helper so that
    # agents cloning or pushing over https://github.com authenticate with the
    # user's gh token instead of prompting. --force lets this run at image-build
    # time, before the sandbox has any authenticated gh host.
    run_as_managed_user gh auth setup-git --hostname github.com --force
}

configure_managed_user

if [[ $(id -u "$managed_user") != 1000 || $(id -g "$managed_user") != 1000 ]]; then
    printf 'managed user %s must have numeric UID/GID 1000 for container device mappings\n' "$managed_user" >&2
    exit 1
fi

install_nvm() {
    local nvm_version=v0.40.6

    curl -fsSL "https://raw.githubusercontent.com/nvm-sh/nvm/$nvm_version/install.sh" |
        run_as_managed_user env \
            NVM_DIR="$managed_home/.nvm" \
            PROFILE="$managed_home/.zshrc" \
            bash
}

install_pi() {
    run_as_managed_user env NVM_DIR="$managed_home/.nvm" bash <<'EOF'
set -Eeuo pipefail

# shellcheck source=/dev/null
source "$NVM_DIR/nvm.sh"
nvm install 22
nvm alias default 22
nvm use --silent default

npm install --global --ignore-scripts \
    @earendil-works/pi-coding-agent@0.83.0

EOF

    install -d -m 0755 -o "$managed_user" -g "$managed_user" \
        "$managed_home/.pi/agent"
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        "$pi_settings_file" "$managed_home/.pi/agent/settings.json"
    install -m 0600 -o "$managed_user" -g "$managed_user" \
        "$pi_auth_file" "$managed_home/.pi/agent/auth.json"
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        "$pi_models_file" "$managed_home/.pi/agent/models.json"
}

install_pi_extension() {
    rm -rf /opt/kanedias/pi-extension
    install -d -m 0755 /opt/kanedias/pi-extension
    cp -a "$pi_extension_dir/." /opt/kanedias/pi-extension/
    chown -R root:root /opt/kanedias/pi-extension
    find /opt/kanedias/pi-extension -type d -exec chmod 0755 {} +
    find /opt/kanedias/pi-extension -type f -exec chmod 0644 {} +

    (
        export NVM_DIR="$managed_home/.nvm"
        # shellcheck source=/dev/null
        source "$NVM_DIR/nvm.sh"
        nvm use --silent default
        cd /opt/kanedias/pi-extension
        npm ci --omit=dev --ignore-scripts
    )

    install -d -m 0755 /usr/lib/tmpfiles.d
    cat > /usr/lib/tmpfiles.d/kanedias.conf <<EOF
d /run/kanedias 0700 kanedias kanedias -
d /run/kanedias-pi 0700 root root -
EOF
    systemd-tmpfiles --create /usr/lib/tmpfiles.d/kanedias.conf

    install -d -m 0755 -o "$managed_user" -g "$managed_user" \
        "$managed_home/.pi/agent/skills/delegate-session" \
        "$managed_home/.pi/agent/skills/writer-handoff"
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        /opt/kanedias/pi-extension/skills/delegate-session/SKILL.md \
        "$managed_home/.pi/agent/skills/delegate-session/SKILL.md"
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        /opt/kanedias/pi-extension/skills/writer-handoff/SKILL.md \
        "$managed_home/.pi/agent/skills/writer-handoff/SKILL.md"
}

install_pi_rpc_service() {
    install -d -m 0755 /usr/local/libexec
    install -m 0755 "$pi_environment_bridge_file" /usr/local/libexec/kanedias-pi-env
    install -m 0755 "$pi_rpc_launcher_file" /usr/local/libexec/kanedias-pi-rpc
    install -m 0644 "$assets_dir/kanedias-pi.socket" \
        /etc/systemd/system/kanedias-pi.socket
    install -m 0644 "$assets_dir/kanedias-pi@.service" \
        /etc/systemd/system/kanedias-pi@.service
    systemctl enable kanedias-pi.socket
}

install_nvm
install_pi
install_pi_extension
install_pi_rpc_service
