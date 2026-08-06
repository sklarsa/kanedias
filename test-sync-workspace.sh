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
printf 'file://%s\n' "$remote_repo" > "$repo_list"

"$sync_script" --inside-instance "$repo_list" "$workspace"
[[ $(<"$workspace/example/version.txt") == one ]]
git -C "$workspace/example" rev-parse --verify refs/tags/obsolete >/dev/null
git -C "$remote_repo" update-ref -d refs/tags/obsolete

printf 'dirty\n' > "$workspace/example/version.txt"
printf 'remove me\n' > "$workspace/example/generated"
printf 'two\n' > "$source_repo/version.txt"
git -C "$source_repo" commit --quiet -am update
git -C "$source_repo" push --quiet "$remote_repo" main

"$sync_script" --inside-instance "$repo_list" "$workspace"
[[ $(<"$workspace/example/version.txt") == two ]]
[[ ! -e $workspace/example/generated ]]
[[ -z $(git -C "$workspace/example" status --short) ]]
if git -C "$workspace/example" rev-parse --verify refs/tags/obsolete \
    >/dev/null 2>&1; then
    fail 'tag deleted from remote was not pruned locally'
fi

printf 'Testing rejection of repository name collisions...\n'
duplicate_list="$temp_dir/duplicate-repos.txt"
duplicate_workspace="$temp_dir/duplicate-workspace"
printf 'file://%s\nfile://%s\n' "$remote_repo" "$remote_repo" \
    > "$duplicate_list"
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
fake_bin="$temp_dir/bin"
fake_home="$temp_dir/home"
incus_log="$temp_dir/incus.log"
mkdir -p "$fake_bin" "$fake_home/.ssh"
cat > "$fake_bin/incus" <<'FAKE_INCUS'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >> "$FAKE_INCUS_LOG"
if [[ $* == 'storage volume show default agent-workspace-seed' ]]; then
    [[ ${FAKE_VOLUME_EXISTS:-0} == 1 ]]
    exit
fi
if [[ -n ${FAKE_FAIL_PREFIX:-} && $* == "$FAKE_FAIL_PREFIX"* ]]; then
    exit 1
fi
exit 0
FAKE_INCUS
chmod +x "$fake_bin/incus"

PATH="$fake_bin:$PATH" HOME="$fake_home" FAKE_INCUS_LOG="$incus_log" \
    INCUS_REPOS_FILE="$repo_list" "$sync_script" test-image

grep -Fxq 'storage volume create default agent-workspace-seed' "$incus_log"
grep -Eq '^config device add workspace-sync-[^ ]+ workspace disk pool=default source=agent-workspace-seed path=/workspace$' "$incus_log"
if grep -Fq 'host-ssh' "$incus_log"; then
    fail 'host SSH directory was exposed to the temporary instance'
fi
grep -Eq '^config device remove workspace-sync-[^ ]+ workspace$' "$incus_log"
grep -Eq '^delete --force workspace-sync-[^ ]+$' "$incus_log"
remove_line=$(grep -nE '^config device remove workspace-sync-[^ ]+ workspace$' "$incus_log" | tail -1 | cut -d: -f1)
delete_line=$(grep -nE '^delete --force workspace-sync-[^ ]+$' "$incus_log" | tail -1 | cut -d: -f1)
(( remove_line < delete_line )) || fail 'workspace volume was not detached before instance deletion'

: > "$incus_log"
PATH="$fake_bin:$PATH" HOME="$fake_home" FAKE_INCUS_LOG="$incus_log" \
    INCUS_REPOS_FILE="$repo_list" FAKE_VOLUME_EXISTS=1 \
    "$sync_script" test-image
if grep -Fq 'storage volume create' "$incus_log"; then
    fail 'existing workspace volume was created again'
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
