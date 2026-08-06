#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
remove_script="$script_dir/remove_sandbox.sh"
temp_dir=$(mktemp -d)
trap 'rm -rf "$temp_dir"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

fake_bin="$temp_dir/bin"
incus_log="$temp_dir/incus.log"
instance_state="$temp_dir/instance"
volume_state="$temp_dir/volume"
workspace_source="$temp_dir/workspace-source"
mkdir -p "$fake_bin"

cat > "$fake_bin/incus" <<'FAKE_INCUS'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$FAKE_INCUS_LOG"

case "${1:-} ${2:-} ${3:-}" in
    'list --format csv')
        [[ $* == 'list --format csv -c n' ]]
        if [[ ${FAKE_FAIL_INSTANCE_LOOKUP:-0} == 1 ]]; then
            echo 'Error: failed to query Incus instances' >&2
            exit 1
        fi
        [[ -z ${FAKE_INSTANCE_EXTRA:-} ]] || printf '%s\n' "$FAKE_INSTANCE_EXTRA"
        if [[ -e $FAKE_INSTANCE_STATE ]]; then
            printf '%s\n' "$FAKE_INSTANCE_NAME"
        fi
        ;;
    'config device get')
        [[ -e $FAKE_INSTANCE_STATE && -e $FAKE_WORKSPACE_SOURCE ]]
        cat "$FAKE_WORKSPACE_SOURCE"
        ;;
    'delete --force '*)
        [[ -e $FAKE_INSTANCE_STATE ]]
        if [[ ${FAKE_FAIL_INSTANCE_DELETE:-0} == 1 ]]; then
            exit 1
        fi
        rm -f "$FAKE_INSTANCE_STATE"
        ;;
    'storage volume list')
        [[ $* == 'storage volume list default --format csv -c t,n' ]]
        if [[ ${FAKE_FAIL_VOLUME_LOOKUP:-0} == 1 ]]; then
            echo 'Error: failed to query Incus storage volumes' >&2
            exit 1
        fi
        [[ -z ${FAKE_VOLUME_EXTRA:-} ]] || printf '%s\n' "$FAKE_VOLUME_EXTRA"
        if [[ -e $FAKE_VOLUME_STATE ]]; then
            printf 'custom,%s\n' "$FAKE_VOLUME_NAME"
        fi
        ;;
    'storage volume delete')
        [[ -e $FAKE_VOLUME_STATE ]]
        [[ ! -e $FAKE_INSTANCE_STATE ]]
        if [[ ${FAKE_FAIL_VOLUME_DELETE:-0} == 1 ]]; then
            exit 1
        fi
        rm -f "$FAKE_VOLUME_STATE"
        ;;
    *)
        echo "unexpected incus command: $*" >&2
        exit 1
        ;;
esac
FAKE_INCUS
chmod +x "$fake_bin/incus"

run_remove() {
    local name=${1:-}
    PATH="$fake_bin:$PATH" \
        KANEDIAS_SANDBOX_LOCK_DIR="$temp_dir/locks" \
        FAKE_INCUS_LOG="$incus_log" \
        FAKE_INSTANCE_STATE="$instance_state" \
        FAKE_INSTANCE_NAME="$name" \
        FAKE_VOLUME_STATE="$volume_state" \
        FAKE_VOLUME_NAME="agent-workspace-$name" \
        FAKE_WORKSPACE_SOURCE="$workspace_source" \
        "$remove_script" "$@"
}

reset_state() {
    : > "$incus_log"
    rm -f "$instance_state" "$volume_state" "$workspace_source"
}

printf 'Testing argument validation...\n'
reset_state
if run_remove; then
    fail 'missing instance name was accepted'
fi
[[ ! -s $incus_log ]] || fail 'Incus was called for invalid arguments'
if run_remove '' || run_remove one two; then
    fail 'empty or multiple instance arguments were accepted'
fi
[[ ! -s $incus_log ]] || fail 'Incus was called for invalid arguments'

printf 'Testing seed volume protection...\n'
reset_state
: > "$volume_state"
if run_remove seed; then
    fail 'instance name that maps to the seed volume was accepted'
fi
[[ -e $volume_state ]] || fail 'seed volume was deleted'
[[ ! -s $incus_log ]] || fail 'Incus was called for protected seed volume'

printf 'Testing normal instance and volume removal...\n'
reset_state
: > "$instance_state"
: > "$volume_state"
printf 'agent-workspace-personal-sandbox\n' > "$workspace_source"
run_remove personal-sandbox
[[ ! -e $instance_state && ! -e $volume_state ]] ||
    fail 'sandbox resources were not removed'
grep -Fxq 'config device get personal-sandbox workspace source' "$incus_log" ||
    fail 'workspace ownership was not verified'
grep -Fxq 'delete --force personal-sandbox' "$incus_log" ||
    fail 'sandbox instance was not deleted'
grep -Fxq 'storage volume delete default agent-workspace-personal-sandbox' "$incus_log" ||
    fail 'sandbox workspace was not deleted'
instance_delete_line=$(grep -nF 'delete --force personal-sandbox' "$incus_log" | cut -d: -f1)
volume_delete_line=$(grep -nF 'storage volume delete default agent-workspace-personal-sandbox' "$incus_log" | cut -d: -f1)
(( instance_delete_line < volume_delete_line )) ||
    fail 'workspace was deleted before its instance'

printf 'Testing mismatched workspace refusal...\n'
reset_state
: > "$instance_state"
: > "$volume_state"
printf 'agent-workspace-someone-else\n' > "$workspace_source"
if run_remove personal-sandbox; then
    fail 'mismatched workspace was accepted'
fi
[[ -e $instance_state && -e $volume_state ]] ||
    fail 'resources were deleted despite workspace mismatch'
if grep -Eq '^(delete --force|storage volume delete)' "$incus_log"; then
    fail 'delete was attempted despite workspace mismatch'
fi

printf 'Testing missing local workspace refusal...\n'
reset_state
: > "$instance_state"
: > "$volume_state"
if run_remove personal-sandbox; then
    fail 'instance without local workspace override was accepted'
fi
[[ -e $instance_state && -e $volume_state ]] ||
    fail 'resources were deleted without ownership proof'

printf 'Testing operational instance lookup failure...\n'
reset_state
: > "$instance_state"
: > "$volume_state"
printf 'agent-workspace-personal-sandbox\n' > "$workspace_source"
if FAKE_FAIL_INSTANCE_LOOKUP=1 run_remove personal-sandbox; then
    fail 'failed instance lookup was treated as absence'
fi
[[ -e $instance_state && -e $volume_state ]] ||
    fail 'resource was deleted after failed instance lookup'
if grep -Eq '^(delete --force|storage volume delete)' "$incus_log"; then
    fail 'delete was attempted after failed instance lookup'
fi

printf 'Testing per-instance lifecycle lock...\n'
reset_state
: > "$instance_state"
: > "$volume_state"
printf 'agent-workspace-personal-sandbox\n' > "$workspace_source"
mkdir -p "$temp_dir/locks"
exec 8> "$temp_dir/locks/personal-sandbox.lock"
flock -n 8
if run_remove personal-sandbox; then
    fail 'concurrent lifecycle operation was accepted'
fi
flock -u 8
[[ -e $instance_state && -e $volume_state ]] ||
    fail 'resource was deleted despite lifecycle lock contention'
[[ ! -s $incus_log ]] || fail 'Incus was called before lifecycle lock acquisition'

printf 'Testing failed instance deletion preserves volume...\n'
reset_state
: > "$instance_state"
: > "$volume_state"
printf 'agent-workspace-personal-sandbox\n' > "$workspace_source"
if FAKE_FAIL_INSTANCE_DELETE=1 run_remove personal-sandbox; then
    fail 'failed instance deletion was reported as success'
fi
[[ -e $instance_state && -e $volume_state ]] ||
    fail 'volume was removed after failed instance deletion'
if grep -Fq 'storage volume delete' "$incus_log"; then
    fail 'volume deletion was attempted after instance deletion failed'
fi

printf 'Testing orphaned volume cleanup...\n'
reset_state
: > "$volume_state"
run_remove abandoned-sandbox
[[ ! -e $volume_state ]] || fail 'orphaned workspace was not deleted'
if grep -Fq 'delete --force' "$incus_log"; then
    fail 'missing instance was deleted'
fi
grep -Fxq 'storage volume delete default agent-workspace-abandoned-sandbox' "$incus_log" ||
    fail 'wrong orphaned workspace name was used'

printf 'Testing missing resources are idempotent...\n'
reset_state
run_remove absent-sandbox
if grep -Eq '^(delete --force|storage volume delete)' "$incus_log"; then
    fail 'delete was called for missing resources'
fi

printf 'Testing exact structured lookup matching...\n'
reset_state
FAKE_INSTANCE_EXTRA=absent-sandbox-old \
    FAKE_VOLUME_EXTRA=$'custom,agent-workspace-absent-sandbox-old\ncontainer,agent-workspace-absent-sandbox' \
    run_remove absent-sandbox
if grep -Eq '^(delete --force|storage volume delete)' "$incus_log"; then
    fail 'near-match or non-custom resource was deleted'
fi

printf 'Testing operational volume lookup failure...\n'
reset_state
: > "$volume_state"
if FAKE_FAIL_VOLUME_LOOKUP=1 run_remove abandoned-sandbox; then
    fail 'failed volume lookup was treated as absence'
fi
[[ -e $volume_state ]] || fail 'volume was deleted after failed lookup'
if grep -Fq 'storage volume delete' "$incus_log"; then
    fail 'volume deletion was attempted after failed lookup'
fi

printf 'Testing volume deletion failure is reported...\n'
reset_state
: > "$volume_state"
if FAKE_FAIL_VOLUME_DELETE=1 run_remove abandoned-sandbox; then
    fail 'failed volume deletion was reported as success'
fi
[[ -e $volume_state ]] || fail 'failed volume deletion removed state'

printf 'PASS: sandbox removal is ordered, idempotent, and fails closed\n'
