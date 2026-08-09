#!/usr/bin/env bash
set -Eeuo pipefail

if (( EUID != 0 )); then
    echo "$(basename "$0") must run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

managed_user=kanedias

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

install_container_tools
