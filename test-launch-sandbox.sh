#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
launch_script="$script_dir/launch-sandbox.sh"
sandbox_profile="$script_dir/profiles/sandbox.yaml"
temp_dir=$(mktemp -d)
trap 'rm -rf "$temp_dir"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

fake_bin="$temp_dir/bin"
incus_log="$temp_dir/incus.log"
go_log="$temp_dir/go.log"
profile_state="$temp_dir/profile-exists"
profile_input="$temp_dir/profile.yaml"
updated_state="$temp_dir/ca-updated"
ready_attempts="$temp_dir/ready-attempts"
volume_state="$temp_dir/volume"
volume_deleted="$temp_dir/volume-deleted"
override_state="$temp_dir/workspace-overridden"
mkdir -p "$fake_bin"

cat > "$fake_bin/go" <<'FAKE_GO'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'cwd=%s args=%s\n' "$PWD" "$*" >> "$FAKE_GO_LOG"
[[ $* == 'run ./proxy -init-ca' ]]
FAKE_GO

cat > "$fake_bin/incus" <<'FAKE_INCUS'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$FAKE_INCUS_LOG"

case "${1:-} ${2:-}" in
    'profile show')
        [[ -e $FAKE_PROFILE_STATE ]]
        ;;
    'profile create')
        : > "$FAKE_PROFILE_STATE"
        ;;
    'profile edit')
        cat > "$FAKE_PROFILE_INPUT"
        ;;
    'storage volume')
        case "${3:-}" in
            copy)
                if [[ ${FAKE_FAIL_COPY:-0} == 1 || -e $FAKE_VOLUME_STATE ]]; then
                    exit 1
                fi
                printf '%s\n' "${5:-}" > "$FAKE_VOLUME_STATE"
                ;;
            delete)
                [[ -e $FAKE_VOLUME_STATE ]]
                [[ ! -e $FAKE_INSTANCE_CREATED ]]
                rm -f "$FAKE_VOLUME_STATE"
                : > "$FAKE_VOLUME_DELETED"
                ;;
            *)
                echo "unexpected incus storage volume command: $*" >&2
                exit 1
                ;;
        esac
        ;;
    'config device')
        [[ ${3:-} == override ]]
        [[ -e $FAKE_INSTANCE_CREATED && -e $FAKE_VOLUME_STATE ]]
        if [[ ${FAKE_FAIL_OVERRIDE:-0} == 1 ]]; then
            exit 1
        fi
        : > "$FAKE_OVERRIDE_STATE"
        ;;
    'init '*)
        if [[ ${FAKE_FAIL_INIT:-0} == 1 ]]; then
            exit 1
        fi
        : > "$FAKE_INSTANCE_CREATED"
        ;;
    'start '*)
        [[ -e $FAKE_INSTANCE_CREATED && -e $FAKE_OVERRIDE_STATE ]]
        if [[ ${FAKE_FAIL_START:-0} == 1 ]]; then
            exit 1
        fi
        ;;
    'exec '*)
        if [[ $* == *' -- systemctl is-system-running --wait' ]]; then
            attempts=0
            [[ ! -f $FAKE_READY_ATTEMPTS ]] || attempts=$(<"$FAKE_READY_ATTEMPTS")
            attempts=$((attempts + 1))
            printf '%s\n' "$attempts" > "$FAKE_READY_ATTEMPTS"
            if (( attempts < ${FAKE_READY_AFTER:-1} )); then
                exit 1
            fi
            echo running
            exit 0
        fi
        if [[ $* != *' -- update-ca-certificates' ]]; then
            echo "unexpected incus exec command: $*" >&2
            exit 1
        fi
        if [[ ${FAKE_FAIL_UPDATE:-0} == 1 ]]; then
            exit 1
        fi
        : > "$FAKE_UPDATED_STATE"
        ;;
    'delete --force')
        rm -f "$FAKE_INSTANCE_CREATED"
        : > "$FAKE_INSTANCE_DELETED"
        ;;
    *)
        echo "unexpected incus command: $*" >&2
        exit 1
        ;;
esac
FAKE_INCUS
chmod +x "$fake_bin/go" "$fake_bin/incus"

run_launcher() {
    PATH="$fake_bin:$PATH" \
        KANEDIAS_SANDBOX_LOCK_DIR="$temp_dir/locks" \
        FAKE_GO_LOG="$go_log" \
        FAKE_INCUS_LOG="$incus_log" \
        FAKE_PROFILE_STATE="$profile_state" \
        FAKE_PROFILE_INPUT="$profile_input" \
        FAKE_INSTANCE_CREATED="$temp_dir/instance-created" \
        FAKE_INSTANCE_DELETED="$temp_dir/instance-deleted" \
        FAKE_UPDATED_STATE="$updated_state" \
        FAKE_READY_ATTEMPTS="$ready_attempts" \
        FAKE_VOLUME_STATE="$volume_state" \
        FAKE_VOLUME_DELETED="$volume_deleted" \
        FAKE_OVERRIDE_STATE="$override_state" \
        "$launch_script" "$@"
}

reset_state() {
    : > "$incus_log"
    : > "$go_log"
    rm -f "$profile_state" "$profile_input" "$updated_state" \
        "$ready_attempts" "$volume_state" "$volume_deleted" \
        "$override_state" "$temp_dir/instance-created" \
        "$temp_dir/instance-deleted"
}

printf 'Testing default sandbox launch...\n'
reset_state
FAKE_READY_AFTER=2 run_launcher kanedias-image

grep -Fxq "cwd=$script_dir args=run ./proxy -init-ca" "$go_log" ||
    fail 'proxy CA initialization was not run from the repository root'
grep -Fxq 'profile create sandbox' "$incus_log" ||
    fail 'missing sandbox profile was not created'
grep -Fxq 'profile edit sandbox' "$incus_log" ||
    fail 'sandbox profile was not refreshed'
grep -Fxq 'storage volume copy default/agent-workspace-seed default/agent-workspace-sandbox --volume-only' "$incus_log" ||
    fail 'default workspace was not cloned from the seed'
grep -Fxq 'init kanedias-image sandbox --profile default --profile sandbox' "$incus_log" ||
    fail 'image was not initialized as the default sandbox instance'
grep -Fxq 'config device override sandbox workspace pool=default source=agent-workspace-sandbox path=/workspace' "$incus_log" ||
    fail 'default workspace device was not overridden'
grep -Fxq 'start sandbox' "$incus_log" ||
    fail 'default sandbox instance was not started'
copy_line=$(grep -nF 'storage volume copy ' "$incus_log" | cut -d: -f1)
init_line=$(grep -nF 'init kanedias-image sandbox ' "$incus_log" | cut -d: -f1)
override_line=$(grep -nF 'config device override sandbox workspace ' "$incus_log" | cut -d: -f1)
start_line=$(grep -nF 'start sandbox' "$incus_log" | cut -d: -f1)
(( copy_line < init_line && init_line < override_line && override_line < start_line )) ||
    fail 'workspace clone, override, and startup ordering is unsafe'
grep -Fxq 'exec sandbox -- systemctl is-system-running --wait' "$incus_log" ||
    fail 'launcher did not wait for systemd readiness'
[[ $(<"$ready_attempts") == 2 ]] ||
    fail 'launcher did not retry the systemd readiness check'
grep -Fxq 'exec sandbox -- update-ca-certificates' "$incus_log" ||
    fail 'container CA store was not updated'
ready_line=$(grep -nF 'exec sandbox -- systemctl is-system-running --wait' "$incus_log" | tail -1 | cut -d: -f1)
update_line=$(grep -nF 'exec sandbox -- update-ca-certificates' "$incus_log" | cut -d: -f1)
(( ready_line < update_line )) || fail 'CA update ran before systemd was ready'
[[ -e $updated_state ]] || fail 'CA update did not complete'
[[ -e $temp_dir/instance-created && -e $volume_state ]] ||
    fail 'successful launch did not preserve its instance and workspace'
if grep -Eq '^(delete --force|storage volume delete)' "$incus_log"; then
    fail 'successful launch deleted an owned resource'
fi

[[ -f $sandbox_profile ]] || fail 'sandbox profile is missing'
cmp -s "$sandbox_profile" "$profile_input" ||
    fail 'launcher did not apply profiles/sandbox.yaml'
grep -Fqx '  environment.HTTP_PROXY: "http://10.75.177.1:3128"' "$profile_input" ||
    fail 'sandbox profile has the wrong proxy endpoint'
grep -Fqx '  environment.GH_TOKEN: "container-dummy"' "$profile_input" ||
    fail 'sandbox profile does not provide the dummy GitHub token'
grep -Fqx '  environment.SSL_CERT_FILE: "/etc/ssl/certs/ca-certificates.crt"' "$profile_input" ||
    fail 'sandbox profile has the wrong system CA bundle path'
grep -Fqx '    pool: default' "$profile_input" ||
    fail 'workspace storage pool is missing'
grep -Fqx '    source: agent-workspace-seed' "$profile_input" ||
    fail 'workspace volume is missing'
grep -Fqx '    source: /home/steven/.config/kanedias-proxy/ca.crt' "$profile_input" ||
    fail 'proxy CA source path is wrong'
if [[ $(grep -Fxc '    type: disk' "$profile_input") != 2 ]]; then
    fail 'sandbox disk devices are malformed'
fi
grep -Fqx '    readonly: "true"' "$profile_input" ||
    fail 'proxy CA mount is not read-only'

printf 'Testing custom instance name and existing profile...\n'
reset_state
: > "$profile_state"
run_launcher kanedias-image personal-sandbox
if grep -Fq 'profile create sandbox' "$incus_log"; then
    fail 'existing sandbox profile was created again'
fi
grep -Fxq 'profile edit sandbox' "$incus_log" ||
    fail 'existing sandbox profile was not refreshed'
grep -Fxq 'storage volume copy default/agent-workspace-seed default/agent-workspace-personal-sandbox --volume-only' "$incus_log" ||
    fail 'custom workspace volume name was not used'
grep -Fxq 'init kanedias-image personal-sandbox --profile default --profile sandbox' "$incus_log" ||
    fail 'custom instance name was not used'
grep -Fxq 'config device override personal-sandbox workspace pool=default source=agent-workspace-personal-sandbox path=/workspace' "$incus_log" ||
    fail 'custom workspace device was not overridden'
grep -Fxq 'start personal-sandbox' "$incus_log" ||
    fail 'custom instance was not started'
grep -Fxq 'exec personal-sandbox -- update-ca-certificates' "$incus_log" ||
    fail 'custom instance CA store was not updated'
[[ -e $temp_dir/instance-created && -e $volume_state ]] ||
    fail 'successful custom launch did not preserve its resources'
if grep -Eq '^(delete --force|storage volume delete)' "$incus_log"; then
    fail 'successful custom launch deleted an owned resource'
fi

printf 'Testing per-instance lifecycle lock...\n'
reset_state
mkdir -p "$temp_dir/locks"
exec 8> "$temp_dir/locks/locked-sandbox.lock"
flock -n 8
if run_launcher kanedias-image locked-sandbox; then
    fail 'concurrent lifecycle operation was accepted'
fi
flock -u 8
[[ ! -s $incus_log && ! -s $go_log ]] ||
    fail 'external commands ran before lifecycle lock acquisition'

printf 'Testing cleanup after CA update failure...\n'
reset_state
: > "$profile_state"
if FAKE_FAIL_UPDATE=1 run_launcher kanedias-image broken-sandbox; then
    fail 'failed CA update was reported as success'
fi
grep -Fxq 'delete --force broken-sandbox' "$incus_log" ||
    fail 'failed sandbox was not deleted'
grep -Fxq 'storage volume delete default agent-workspace-broken-sandbox' "$incus_log" ||
    fail 'failed sandbox workspace was not deleted'
instance_delete_line=$(grep -nF 'delete --force broken-sandbox' "$incus_log" | cut -d: -f1)
volume_delete_line=$(grep -nF 'storage volume delete default agent-workspace-broken-sandbox' "$incus_log" | cut -d: -f1)
(( instance_delete_line < volume_delete_line )) ||
    fail 'workspace was deleted before its instance'

printf 'Testing workspace copy collision safety...\n'
reset_state
: > "$profile_state"
: > "$volume_state"
if run_launcher kanedias-image existing-workspace; then
    fail 'existing workspace volume was reused'
fi
if grep -Fq 'init kanedias-image existing-workspace' "$incus_log"; then
    fail 'instance was initialized after workspace copy collision'
fi
if grep -Fq 'storage volume delete default agent-workspace-existing-workspace' "$incus_log"; then
    fail 'launcher deleted a workspace volume it did not create'
fi
[[ -e $volume_state ]] || fail 'pre-existing workspace volume was removed'

printf 'Testing instance creation collision safety...\n'
reset_state
: > "$profile_state"
if FAKE_FAIL_INIT=1 run_launcher kanedias-image existing-sandbox; then
    fail 'failed instance initialization was reported as success'
fi
if grep -Fq 'delete --force existing-sandbox' "$incus_log"; then
    fail 'launcher deleted an instance it did not create'
fi
grep -Fxq 'storage volume delete default agent-workspace-existing-sandbox' "$incus_log" ||
    fail 'owned workspace was not deleted after instance collision'

printf 'Testing workspace override failure cleanup...\n'
reset_state
: > "$profile_state"
if FAKE_FAIL_OVERRIDE=1 run_launcher kanedias-image override-failed-sandbox; then
    fail 'failed workspace override was reported as success'
fi
grep -Fxq 'delete --force override-failed-sandbox' "$incus_log" ||
    fail 'instance with failed workspace override was not deleted'
grep -Fxq 'storage volume delete default agent-workspace-override-failed-sandbox' "$incus_log" ||
    fail 'workspace with failed device override was not deleted'

printf 'Testing partial startup cleanup...\n'
reset_state
: > "$profile_state"
if FAKE_FAIL_START=1 run_launcher kanedias-image stopped-sandbox; then
    fail 'failed instance startup was reported as success'
fi
grep -Fxq 'delete --force stopped-sandbox' "$incus_log" ||
    fail 'instance left behind by failed startup was not deleted'
grep -Fxq 'storage volume delete default agent-workspace-stopped-sandbox' "$incus_log" ||
    fail 'workspace left behind by failed startup was not deleted'

printf 'PASS: sandbox launcher configures COW workspaces, CA trust, and safe cleanup\n'
