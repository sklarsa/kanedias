#!/usr/bin/env bash
set -Eeuo pipefail

if (( EUID != 0 )); then
    echo "$(basename "$0") must run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

managed_user=kanedias
managed_home=/home/kanedias

run_as_managed_user() (
    cd "$managed_home"
    exec runuser -u "$managed_user" -- \
        env HOME="$managed_home" USER="$managed_user" LOGNAME="$managed_user" \
        "$@"
)

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

apt-get update
apt-get install -y --no-install-recommends clang gcc
install_go
install_pulumi
install_uv
install_tfenv
