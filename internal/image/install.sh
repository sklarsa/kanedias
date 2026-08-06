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
pi_theme_file="$assets_dir/cobalt-ember.json"
tmux_config_file="$assets_dir/tmux.conf"

for required_file in \
    "$authorized_hosts_file" "$pi_settings_file" "$pi_theme_file" \
    "$tmux_config_file"; do
    if [[ ! -f $required_file ]]; then
        echo "missing install input: $required_file" >&2
        exit 1
    fi
done

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends \
    apt-transport-https \
    bind9-dnsutils \
    bsdutils \
    ca-certificates \
    clang \
    coreutils \
    cpio \
    curl \
    dialog \
    diffutils \
    file \
    findutils \
    fd \
    gcc \
    gh \
    git \
    gnupg \
    grep \
    groff \
    gzip \
    hostname \
    htop \
    incus-base \
    iproute2 \
    iputils-ping \
    jq \
    less \
    locales \
    lsb-release \
    make \
    mawk \
    net-tools \
    nodejs \
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

    usermod --append --groups sudo,incus-admin "$managed_user"

    install -m 0440 /dev/null "/etc/sudoers.d/$managed_user"
    printf '%s ALL=(ALL:ALL) NOPASSWD: ALL\n' "$managed_user" \
        > "/etc/sudoers.d/$managed_user"
    visudo -cf "/etc/sudoers.d/$managed_user" >/dev/null

    install -d -m 0700 -o "$managed_user" -g "$managed_user" \
        "$managed_home/.ssh"
    install -m 0600 -o "$managed_user" -g "$managed_user" \
        "$authorized_hosts_file" "$managed_home/.ssh/authorized_keys"
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        "$tmux_config_file" "$managed_home/.tmux.conf"
    if [[ ! -e $managed_home/.zshrc ]]; then
        install -m 0644 -o "$managed_user" -g "$managed_user" \
            /dev/null "$managed_home/.zshrc"
    fi

    printf '%s\n' "export PATH=\"\$HOME/.local/bin:\$PATH\"" \
        > "$managed_home/.zshenv"
    chown "$managed_user:$managed_user" "$managed_home/.zshenv"
    chmod 0644 "$managed_home/.zshenv"
}

configure_managed_user

install_cloud_apt_packages() {
    local architecture
    architecture=$(dpkg --print-architecture)

    case $architecture in
        amd64 | arm64) ;;
        *)
            echo "unsupported architecture for cloud CLIs: $architecture" >&2
            return 1
            ;;
    esac

    mkdir -p /etc/apt/keyrings /usr/share/keyrings

    curl -fsSL https://packages.microsoft.com/keys/microsoft.asc |
        gpg --dearmor --yes -o /etc/apt/keyrings/microsoft.gpg
    chmod a+r /etc/apt/keyrings/microsoft.gpg

    cat > /etc/apt/sources.list.d/azure-cli.sources <<EOF
Types: deb
URIs: https://packages.microsoft.com/repos/azure-cli/
Suites: bookworm
Components: main
Architectures: $architecture
Signed-by: /etc/apt/keyrings/microsoft.gpg
EOF

    curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg |
        gpg --dearmor --yes -o /usr/share/keyrings/cloud.google.gpg

    printf '%s\n' \
        'deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main' \
        > /etc/apt/sources.list.d/google-cloud-sdk.list

    apt-get update
    apt-get install -y --no-install-recommends \
        azure-cli \
        google-cloud-cli \
        google-cloud-cli-gke-gcloud-auth-plugin
}

install_aws_cli() {
    curl -fsSL https://awscli.amazonaws.com/v2/install.sh |
        bash -s -- --system
}

install_session_manager_plugin() (
    set -Eeuo pipefail

    local architecture plugin_url temp_dir
    architecture=$(dpkg --print-architecture)

    case $architecture in
        amd64)
            plugin_url='https://s3.amazonaws.com/session-manager-downloads/plugin/latest/ubuntu_64bit/session-manager-plugin.deb'
            ;;
        arm64)
            plugin_url='https://s3.amazonaws.com/session-manager-downloads/plugin/latest/ubuntu_arm64/session-manager-plugin.deb'
            ;;
        *)
            echo "unsupported architecture for Session Manager plugin: $architecture" >&2
            return 1
            ;;
    esac

    temp_dir=$(mktemp -d)
    trap 'rm -rf "$temp_dir"' EXIT

    curl -fsSL --retry 3 "$plugin_url" -o "$temp_dir/session-manager-plugin.deb"
    curl -fsSL --retry 3 "$plugin_url.sig" -o "$temp_dir/session-manager-plugin.deb.sig"

    cat > "$temp_dir/session-manager-plugin.gpg" <<'EOF'
-----BEGIN PGP PUBLIC KEY BLOCK-----

mFIEZ5ERQxMIKoZIzj0DAQcCAwQjuZy+IjFoYg57sLTGhF3aZLBaGpzB+gY6j7Ix
P7NqbpXyjVj8a+dy79gSd64OEaMxUb7vw/jug+CfRXwVGRMNtIBBV1MgU1NNIFNl
c3Npb24gTWFuYWdlciA8c2Vzc2lvbi1tYW5hZ2VyLXBsdWdpbi1zaWduZXJAYW1h
em9uLmNvbT4gKEFXUyBTeXN0ZW1zIE1hbmFnZXIgU2Vzc2lvbiBNYW5hZ2VyIFBs
dWdpbiBMaW51eCBTaWduZXIgS2V5KYkBAAQQEwgAqAUCZ5ERQ4EcQVdTIFNTTSBT
ZXNzaW9uIE1hbmFnZXIgPHNlc3Npb24tbWFuYWdlci1wbHVnaW4tc2lnbmVyQGFt
YXpvbi5jb20+IChBV1MgU3lzdGVtcyBNYW5hZ2VyIFNlc3Npb24gTWFuYWdlciBQ
bHVnaW4gTGludXggU2lnbmVyIEtleSkWIQR5WWNxJM4JOtUB1HosTUr/b2dX7gIe
AwIbAwIVCAAKCRAsTUr/b2dX7rO1AQCa1kig3lQ78W/QHGU76uHx3XAyv0tfpE9U
oQBCIwFLSgEA3PDHt3lZ+s6m9JLGJsy+Cp5ZFzpiF6RgluR/2gA861M=
=2DQm
-----END PGP PUBLIC KEY BLOCK-----
EOF

    mkdir -m 700 "$temp_dir/gnupg"
    export GNUPGHOME="$temp_dir/gnupg"

    fingerprint=$(
        gpg --batch --show-keys --with-colons "$temp_dir/session-manager-plugin.gpg" |
            awk -F: '$1 == "fpr" { print $10; exit }'
    )
    [[ $fingerprint == 7959637124CE093AD501D47A2C4D4AFF6F6757EE ]]

    gpg --batch --import "$temp_dir/session-manager-plugin.gpg"
    gpg --batch --verify \
        "$temp_dir/session-manager-plugin.deb.sig" \
        "$temp_dir/session-manager-plugin.deb"

    dpkg -i "$temp_dir/session-manager-plugin.deb"
)

install_container_tools() {
    local architecture kind_asset kind_checksum kind_tag k9s_asset k9s_checksum
    local k9s_tag temp_dir

    architecture=$(dpkg --print-architecture)
    case $architecture in
        amd64 | arm64) ;;
        *)
            echo "unsupported architecture for container tools: $architecture" >&2
            return 1
            ;;
    esac

    touch /.dockerenv

    for package in \
        docker.io docker-compose docker-doc docker-buildx podman-docker \
        containerd runc; do
        if dpkg-query -W "$package" >/dev/null 2>&1; then
            apt-get remove -y "$package"
        fi
    done

    apt-get install -y --no-install-recommends podman

    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/debian/gpg \
        -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc

    cat > /etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/debian
Suites: trixie
Components: stable
Architectures: $architecture
Signed-By: /etc/apt/keyrings/docker.asc
EOF

    apt-get update
    apt-get install -y --no-install-recommends \
        containerd.io \
        docker-buildx-plugin \
        docker-ce \
        docker-ce-cli \
        docker-compose-plugin
    systemctl enable --now docker
    usermod --append --groups docker "$managed_user"

    temp_dir=$(mktemp -d)

    k9s_tag=$(
        curl -fsSL \
            -H 'Accept: application/vnd.github+json' \
            https://api.github.com/repos/derailed/k9s/releases/latest |
            jq -er .tag_name
    )
    k9s_asset="k9s_Linux_${architecture}.tar.gz"
    curl -fsSL \
        "https://github.com/derailed/k9s/releases/download/$k9s_tag/$k9s_asset" \
        -o "$temp_dir/$k9s_asset"
    curl -fsSL \
        "https://github.com/derailed/k9s/releases/download/$k9s_tag/checksums.sha256" \
        -o "$temp_dir/k9s-checksums.sha256"
    k9s_checksum=$(
        awk -v file="$k9s_asset" \
            '$2 == file || $2 == "*" file { print $1 }' \
            "$temp_dir/k9s-checksums.sha256"
    )
    [[ $k9s_checksum =~ ^[[:xdigit:]]{64}$ ]]
    printf '%s  %s\n' "$k9s_checksum" "$temp_dir/$k9s_asset" |
        sha256sum --check -
    tar -C "$temp_dir" -xzf "$temp_dir/$k9s_asset" k9s
    install -m 0755 "$temp_dir/k9s" /usr/local/bin/k9s

    kind_tag=$(
        curl -fsSL \
            -H 'Accept: application/vnd.github+json' \
            https://api.github.com/repos/kubernetes-sigs/kind/releases/latest |
            jq -er .tag_name
    )
    kind_asset="kind-linux-${architecture}"
    curl -fsSL \
        "https://github.com/kubernetes-sigs/kind/releases/download/$kind_tag/$kind_asset" \
        -o "$temp_dir/$kind_asset"
    curl -fsSL \
        "https://github.com/kubernetes-sigs/kind/releases/download/$kind_tag/$kind_asset.sha256sum" \
        -o "$temp_dir/$kind_asset.sha256sum"
    kind_checksum=$(awk '{ print $1 }' "$temp_dir/$kind_asset.sha256sum")
    [[ $kind_checksum =~ ^[[:xdigit:]]{64}$ ]]
    printf '%s  %s\n' "$kind_checksum" "$temp_dir/$kind_asset" |
        sha256sum --check -
    install -m 0755 "$temp_dir/$kind_asset" /usr/local/bin/kind

    install -d /etc/kind
    cat > /etc/kind/config.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        apiVersion: kubelet.config.k8s.io/v1beta1
        kind: KubeletConfiguration
        featureGates:
          KubeletInUserNamespace: true
  - role: worker
  - role: worker
EOF

    rm -rf "$temp_dir"
}

install_claude_code() {
    local fingerprint

    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://downloads.claude.ai/keys/claude-code.asc \
        -o /etc/apt/keyrings/claude-code.asc

    fingerprint=$(
        gpg --batch --show-keys --with-colons \
            /etc/apt/keyrings/claude-code.asc |
            awk -F: '$1 == "fpr" { print $10; exit }'
    )
    [[ $fingerprint == 31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE ]]

    printf '%s\n' \
        'deb [signed-by=/etc/apt/keyrings/claude-code.asc] https://downloads.claude.ai/claude-code/apt/latest latest main' \
        > /etc/apt/sources.list.d/claude-code.list

    apt-get update
    apt-get install -y --no-install-recommends claude-code
}

install_go() {
    local go_arch go_archive go_checksum go_filename go_version metadata_dir

    case $(dpkg --print-architecture) in
        amd64 | arm64) go_arch=$(dpkg --print-architecture) ;;
        *)
            echo "unsupported architecture for Go: $(dpkg --print-architecture)" >&2
            return 1
            ;;
    esac

    metadata_dir=$(mktemp -d)
    curl -fsSL 'https://go.dev/dl/?mode=json' -o "$metadata_dir/releases.json"

    IFS=$'\t' read -r go_version go_filename go_checksum < <(
        jq -er --arg arch "$go_arch" '
            .[0] as $release
            | $release.files[]
            | select(.os == "linux" and .arch == $arch and .kind == "archive")
            | [$release.version, .filename, .sha256]
            | @tsv
        ' "$metadata_dir/releases.json"
    )

    if [[ -x /usr/local/go/bin/go ]] &&
        [[ $(/usr/local/go/bin/go env GOVERSION) == "$go_version" ]]; then
        printf 'Go %s is already installed.\n' "$go_version"
        rm -rf "$metadata_dir"
        return
    fi

    go_archive="$metadata_dir/$go_filename"
    curl -fsSL "https://go.dev/dl/$go_filename" -o "$go_archive"
    printf '%s  %s\n' "$go_checksum" "$go_archive" | sha256sum --check -

    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$go_archive"
    ln -sfn /usr/local/go/bin/go /usr/local/bin/go
    ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt
    rm -rf "$metadata_dir"
}

install_pulumi() {
    curl -fsSL https://get.pulumi.com |
        sh -s -- --install-root /usr/local --no-edit-path
}

install_uv() {
    install -d -m 0755 -o "$managed_user" -g "$managed_user" \
        "$managed_home/.local" "$managed_home/.local/bin"
    curl -LsSf https://astral.sh/uv/install.sh |
        run_as_managed_user env \
            UV_INSTALL_DIR="$managed_home/.local/bin" \
            UV_NO_MODIFY_PATH=1 \
            sh
}

install_tfenv() {
    local command_name tfenv_dir="$managed_home/.tfenv"

    rm -rf "$tfenv_dir"
    run_as_managed_user git clone --depth=1 \
        https://github.com/tfutils/tfenv.git "$tfenv_dir"

    for command_name in terraform tfenv; do
        run_as_managed_user ln -sfn "$tfenv_dir/bin/$command_name" \
            "$managed_home/.local/bin/$command_name"
    done
}

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

pi_binary="$(dirname "$(command -v node)")/pi"
GIT_TERMINAL_PROMPT=0 "$pi_binary" install git:github.com/obra/superpowers
"$pi_binary" install npm:pi-subagents
"$pi_binary" install npm:pi-web-suite
EOF

    install -d -m 0755 -o "$managed_user" -g "$managed_user" \
        "$managed_home/.pi/agent" "$managed_home/.pi/agent/themes"
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        "$pi_settings_file" "$managed_home/.pi/agent/settings.json"
    install -m 0644 -o "$managed_user" -g "$managed_user" \
        "$pi_theme_file" "$managed_home/.pi/agent/themes/cobalt-ember.json"
}

install_cloud_apt_packages
install_aws_cli
install_session_manager_plugin
install_container_tools
install_claude_code
install_go
install_pulumi
install_uv
install_tfenv
install_nvm
install_pi
