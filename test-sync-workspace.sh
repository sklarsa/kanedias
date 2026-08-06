#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
sync_script="$script_dir/sync-workspace.sh"
temp_dir=$(mktemp -d)

cleanup() {
    rm -rf "$temp_dir"
}
trap cleanup EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

[[ -x $sync_script ]] || fail "missing executable $sync_script"

printf 'Testing repository clone and refresh...\n'
source_repo="$temp_dir/source"
remote_repo="$temp_dir/example.git"
workspace="$temp_dir/workspace"
repo_list="$temp_dir/repos.txt"

git init --quiet --initial-branch=main "$source_repo"
git -C "$source_repo" config user.name test
git -C "$source_repo" config user.email test@example.com
printf 'generated\n' > "$source_repo/.gitignore"
printf 'one\n' > "$source_repo/version.txt"
git -C "$source_repo" add .
git -C "$source_repo" commit --quiet -m initial
git -C "$source_repo" tag obsolete
git clone --quiet --bare "$source_repo" "$remote_repo"
printf 'test/example\n' > "$repo_list"

fake_bin="$temp_dir/bin"
gh_log="$temp_dir/gh.log"
git_config="$temp_dir/gitconfig"
mkdir -p "$fake_bin"
cat > "$fake_bin/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_LOG"
if [[ $* == 'auth setup-git --hostname github.com --force' ]]; then
    exit 0
fi
if [[ ${1:-} == repo && ${2:-} == clone ]]; then
    shift 2
    url=$1
    target=$2
    shift 2
    [[ ${1:-} == -- ]]
    shift
    exec git clone "$@" "$url" "$target"
fi
echo "unexpected gh command: $*" >&2
exit 1
FAKE_GH
chmod +x "$fake_bin/gh"
git config --file "$git_config" \
    url."file://$remote_repo".insteadOf \
    https://github.com/test/example.git
export PATH="$fake_bin:$PATH"
export FAKE_GH_LOG="$gh_log"
export GIT_CONFIG_GLOBAL="$git_config"

"$sync_script" --inside-instance "$repo_list" "$workspace"
[[ $(<"$workspace/example/version.txt") == one ]]
git -C "$workspace/example" rev-parse --verify refs/tags/obsolete >/dev/null
grep -Fxq 'auth setup-git --hostname github.com --force' "$gh_log"
grep -Fxq "repo clone https://github.com/test/example.git $workspace/example -- --recurse-submodules" "$gh_log"
[[ $(git -C "$workspace/example" config --get remote.origin.url) == \
    https://github.com/test/example.git ]]
git -C "$remote_repo" update-ref -d refs/tags/obsolete

git -C "$workspace/example" remote set-url origin \
    git@github.com:test/example.git
printf 'dirty\n' > "$workspace/example/version.txt"
printf 'remove me\n' > "$workspace/example/generated"
printf 'two\n' > "$source_repo/version.txt"
git -C "$source_repo" commit --quiet -am update
git -C "$source_repo" push --quiet "$remote_repo" main

"$sync_script" --inside-instance "$repo_list" "$workspace"
[[ $(<"$workspace/example/version.txt") == two ]]
[[ ! -e $workspace/example/generated ]]
[[ -z $(git -C "$workspace/example" status --short) ]]
[[ $(git -C "$workspace/example" config --get remote.origin.url) == \
    https://github.com/test/example.git ]]
if git -C "$workspace/example" rev-parse --verify refs/tags/obsolete \
    >/dev/null 2>&1; then
    fail 'tag deleted from remote was not pruned locally'
fi

printf 'Testing rejection of invalid GitHub repository slugs...\n'
invalid_list="$temp_dir/invalid-repos.txt"
invalid_workspace="$temp_dir/invalid-workspace"
printf 'test/example/extra\n' > "$invalid_list"
if invalid_output=$("$sync_script" --inside-instance \
    "$invalid_list" "$invalid_workspace" 2>&1); then
    fail 'invalid GitHub repository slug was accepted'
fi
if [[ $invalid_output != *'invalid GitHub repository slug: test/example/extra'* ]]; then
    fail "invalid repository failure was unclear: $invalid_output"
fi
[[ ! -e $invalid_workspace ]]

printf 'Testing rejection of repository name collisions...\n'
duplicate_list="$temp_dir/duplicate-repos.txt"
duplicate_workspace="$temp_dir/duplicate-workspace"
printf 'test/example\ntest/example\n' > "$duplicate_list"
if "$sync_script" --inside-instance "$duplicate_list" "$duplicate_workspace"; then
    fail 'duplicate repository destination was accepted'
fi
[[ ! -e $duplicate_workspace/example ]]

printf 'Testing rejection of a symlinked repository root...\n'
symlinked_root_target="$temp_dir/symlinked-root-target"
symlinked_root="$temp_dir/symlinked-root"
mkdir -p "$symlinked_root_target"
ln -s "$symlinked_root_target" "$symlinked_root"
if "$sync_script" --inside-instance "$repo_list" "$symlinked_root"; then
    fail 'symlinked repository root was accepted'
fi
[[ ! -e $symlinked_root_target/example ]]

printf 'Testing rejection of symlinked repository targets...\n'
external_repo="$temp_dir/external"
unsafe_workspace="$temp_dir/unsafe-workspace"
git clone --quiet "$remote_repo" "$external_repo"
printf 'outside change\n' > "$external_repo/version.txt"
printf 'outside untracked\n' > "$external_repo/untracked.txt"
mkdir -p "$unsafe_workspace"
ln -s "$external_repo" "$unsafe_workspace/example"
if "$sync_script" --inside-instance "$repo_list" "$unsafe_workspace"; then
    fail 'symlinked repository target was accepted'
fi
[[ $(<"$external_repo/version.txt") == 'outside change' ]]
[[ -e $external_repo/untracked.txt ]]

printf 'Testing rejection of redirected Git worktrees...\n'
redirected_root="$temp_dir/redirected-root"
redirected_external="$temp_dir/redirected-external"
git clone --quiet "$remote_repo" "$redirected_root/example"
mkdir -p "$redirected_external"
printf 'protected\n' > "$redirected_external/version.txt"
printf 'protected untracked\n' > "$redirected_external/untracked.txt"
git -C "$redirected_root/example" config core.worktree "$redirected_external"
if "$sync_script" --inside-instance "$repo_list" "$redirected_root"; then
    fail 'repository with redirected core.worktree was accepted'
fi
[[ $(<"$redirected_external/version.txt") == protected ]]
[[ -e $redirected_external/untracked.txt ]]

printf 'Testing Incus resource lifecycle...\n'
fake_home="$temp_dir/home"
incus_log="$temp_dir/incus.log"
dns_state="$temp_dir/dns-attempts"
mkdir -p "$fake_home/.ssh"
cat > "$fake_bin/go" <<'FAKE_GO'
#!/usr/bin/env bash
set -u
printf 'go %s\n' "$*" >> "$FAKE_INCUS_LOG"
FAKE_GO
cat > "$fake_bin/incus" <<'FAKE_INCUS'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >> "$FAKE_INCUS_LOG"
if [[ $* == 'storage volume show default agent-workspace-seed' ]]; then
    [[ ${FAKE_VOLUME_EXISTS:-0} == 1 ]]
    exit
fi
if [[ $* == 'profile show sandbox' ]]; then
    [[ ${FAKE_PROFILE_EXISTS:-0} == 1 ]]
    exit
fi
if [[ -n ${FAKE_FAIL_PREFIX:-} && $* == "$FAKE_FAIL_PREFIX"* ]]; then
    exit 1
fi
if [[ $* =~ ^exec\ workspace-sync-[^[:space:]]+\ --\ getent\ ahosts\ github\.com$ ]]; then
    attempts=0
    if [[ -f $FAKE_DNS_STATE ]]; then
        read -r attempts < "$FAKE_DNS_STATE"
    fi
    (( attempts += 1 ))
    printf '%d\n' "$attempts" > "$FAKE_DNS_STATE"
    (( attempts > ${FAKE_DNS_FAILURES:-0} ))
    exit
fi
exit 0
FAKE_INCUS
chmod +x "$fake_bin/go" "$fake_bin/incus"
export FAKE_DNS_STATE="$dns_state"

printf 'Testing DNS timeout validation before Incus mutation...\n'
timeout_marker="$temp_dir/arithmetic-injection"
malicious_timeout="BASH_VERSINFO[\$(touch $timeout_marker)0]"
for invalid_timeout in invalid "$malicious_timeout" 3601; do
    : > "$incus_log"
    rm -f "$timeout_marker" "$dns_state"
    if invalid_timeout_output=$(PATH="$fake_bin:$PATH" HOME="$fake_home" \
        FAKE_INCUS_LOG="$incus_log" INCUS_REPOS_FILE="$repo_list" \
        INCUS_DNS_TIMEOUT="$invalid_timeout" "$sync_script" \
        test-image 2>&1); then
        fail "invalid DNS timeout was accepted: $invalid_timeout"
    fi
    if [[ $invalid_timeout_output != \
        *'INCUS_DNS_TIMEOUT must be an integer from 1 to 3600'* ]]; then
        fail "invalid DNS timeout failure was unclear: $invalid_timeout_output"
    fi
    [[ ! -e $timeout_marker ]] || \
        fail 'DNS timeout arithmetic executed a command substitution'
    [[ ! -s $incus_log ]] || \
        fail 'Incus was mutated before DNS timeout validation'
done

PATH="$fake_bin:$PATH" HOME="$fake_home" FAKE_INCUS_LOG="$incus_log" \
    INCUS_REPOS_FILE="$repo_list" FAKE_DNS_FAILURES=1 \
    "$sync_script" test-image

grep -Fxq 'go run ./proxy -init-ca' "$incus_log"
grep -Fxq 'profile create sandbox' "$incus_log"
grep -Fxq 'profile edit sandbox' "$incus_log"
grep -Fxq 'storage volume create default agent-workspace-seed' "$incus_log"
grep -Eq '^init test-image workspace-sync-[^ ]+ --profile default --profile sandbox$' "$incus_log"
grep -Eq '^config device override workspace-sync-[^ ]+ workspace pool=default source=agent-workspace-seed path=/workspace$' "$incus_log"
[[ $(grep -Ec '^exec workspace-sync-[^ ]+ -- getent ahosts github\.com$' "$incus_log") == 2 ]]
grep -Eq '^exec workspace-sync-[^ ]+ -- update-ca-certificates$' "$incus_log"
if grep -Fq 'host-ssh' "$incus_log"; then
    fail 'host SSH directory was exposed to the temporary instance'
fi
grep -Eq '^config device remove workspace-sync-[^ ]+ workspace$' "$incus_log"
grep -Eq '^delete --force workspace-sync-[^ ]+$' "$incus_log"
go_line=$(grep -nF 'go run ./proxy -init-ca' "$incus_log" | cut -d: -f1)
profile_line=$(grep -nF 'profile edit sandbox' "$incus_log" | cut -d: -f1)
init_line=$(grep -nE '^init test-image workspace-sync-[^ ]+ --profile default --profile sandbox$' "$incus_log" | cut -d: -f1)
override_line=$(grep -nE '^config device override workspace-sync-[^ ]+ workspace ' "$incus_log" | cut -d: -f1)
dns_line=$(grep -nE '^exec workspace-sync-[^ ]+ -- getent ahosts github\.com$' "$incus_log" | head -1 | cut -d: -f1)
ca_line=$(grep -nE '^exec workspace-sync-[^ ]+ -- update-ca-certificates$' "$incus_log" | cut -d: -f1)
sync_line=$(grep -nE '^exec workspace-sync-[^ ]+ -- runuser .* --inside-instance ' "$incus_log" | cut -d: -f1)
(( go_line < profile_line && profile_line < init_line && \
    init_line < override_line && override_line < dns_line && \
    dns_line < ca_line && ca_line < sync_line )) || \
    fail 'profile, DNS, CA, and repository synchronization ordering is wrong'
remove_line=$(grep -nE '^config device remove workspace-sync-[^ ]+ workspace$' "$incus_log" | tail -1 | cut -d: -f1)
delete_line=$(grep -nE '^delete --force workspace-sync-[^ ]+$' "$incus_log" | tail -1 | cut -d: -f1)
(( remove_line < delete_line )) || fail 'workspace volume was not detached before instance deletion'

: > "$incus_log"
rm -f "$dns_state"
PATH="$fake_bin:$PATH" HOME="$fake_home" FAKE_INCUS_LOG="$incus_log" \
    INCUS_REPOS_FILE="$repo_list" FAKE_VOLUME_EXISTS=1 \
    FAKE_PROFILE_EXISTS=1 "$sync_script" test-image
if grep -Fq 'storage volume create' "$incus_log"; then
    fail 'existing workspace volume was created again'
fi
if grep -Fq 'profile create sandbox' "$incus_log"; then
    fail 'existing sandbox profile was created again'
fi
grep -Fxq 'profile edit sandbox' "$incus_log"

printf 'Testing DNS readiness timeout cleanup...\n'
: > "$incus_log"
rm -f "$dns_state"
if dns_output=$(PATH="$fake_bin:$PATH" HOME="$fake_home" \
    FAKE_INCUS_LOG="$incus_log" INCUS_REPOS_FILE="$repo_list" \
    FAKE_VOLUME_EXISTS=1 FAKE_PROFILE_EXISTS=1 FAKE_DNS_FAILURES=100 \
    INCUS_DNS_TIMEOUT=1 "$sync_script" test-image 2>&1); then
    fail 'DNS readiness timeout was reported as success'
fi
if [[ $dns_output != *'Timed out waiting for DNS in '* ]]; then
    fail "DNS timeout failure was unclear: $dns_output"
fi
grep -Eq '^delete --force workspace-sync-[^ ]+$' "$incus_log"
if grep -Eq '^exec workspace-sync-[^ ]+ -- runuser .* --inside-instance ' \
    "$incus_log"; then
    fail 'repository synchronization ran before DNS became ready'
fi

printf 'Testing cleanup failure reporting...\n'
: > "$incus_log"
if PATH="$fake_bin:$PATH" HOME="$fake_home" FAKE_INCUS_LOG="$incus_log" \
    INCUS_REPOS_FILE="$repo_list" FAKE_VOLUME_EXISTS=1 \
    FAKE_FAIL_PREFIX='config device remove ' "$sync_script" test-image; then
    fail 'device detach failure was reported as success'
fi
grep -Eq '^delete --force workspace-sync-[^ ]+$' "$incus_log"

printf 'Testing instance creation collision safety...\n'
: > "$incus_log"
if PATH="$fake_bin:$PATH" HOME="$fake_home" FAKE_INCUS_LOG="$incus_log" \
    INCUS_REPOS_FILE="$repo_list" FAKE_VOLUME_EXISTS=1 \
    FAKE_FAIL_PREFIX='init ' "$sync_script" test-image; then
    fail 'failed instance creation was reported as success'
fi
if grep -Eq '^delete --force workspace-sync-[^ ]+$' "$incus_log"; then
    fail 'instance creation failure deleted an instance the script did not own'
fi

printf 'Testing partial startup cleanup...\n'
: > "$incus_log"
if PATH="$fake_bin:$PATH" HOME="$fake_home" FAKE_INCUS_LOG="$incus_log" \
    INCUS_REPOS_FILE="$repo_list" FAKE_VOLUME_EXISTS=1 \
    FAKE_FAIL_PREFIX='start ' "$sync_script" test-image; then
    fail 'failed instance startup was reported as success'
fi
grep -Eq '^delete --force workspace-sync-[^ ]+$' "$incus_log" || \
    fail 'failed instance startup did not clean up the created instance'

printf 'PASS: workspace sync is idempotent and cleans up Incus resources\n'
