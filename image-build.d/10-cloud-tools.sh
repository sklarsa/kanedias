#!/usr/bin/env bash
set -Eeuo pipefail

if (( EUID != 0 )); then
    echo "$(basename "$0") must run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

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

install_cloud_apt_packages
install_aws_cli
install_session_manager_plugin
