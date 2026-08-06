# Proxied Workspace Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Synchronize the workspace seed from GitHub through the sandbox credential proxy using GitHub CLI over HTTPS after DNS becomes ready.

**Architecture:** `sync-workspace.sh` will treat repository entries as `owner/repository` slugs, derive canonical HTTPS URLs, configure `gh` as Git's credential helper, and use `gh repo clone` for missing repositories. Its outer lifecycle will mirror `launch-sandbox.sh` by initializing the proxy CA and sandbox profile, applying `default + sandbox`, overriding the workspace device, waiting for DNS, and updating trusted CAs before syncing.

**Tech Stack:** Bash, Incus 7.2, Git, GitHub CLI, existing Go credential proxy

## Global Constraints

- Repository entries use exactly one `owner/repository` separator.
- GitHub remotes use `https://github.com/owner/repository.git`.
- DNS readiness polls `getent ahosts github.com` for at most 60 seconds by default.
- The long-running proxy remains externally managed.
- The existing workspace safety checks and cleanup behavior remain in force.
- The unrelated working-tree change in `build-image.sh` must not be staged or modified.

---

### Task 1: GitHub slug and HTTPS repository synchronization

**Files:**
- Modify: `test-sync-workspace.sh:10-103`
- Modify: `sync-workspace.sh:6-81`

**Interfaces:**
- Consumes: repository list lines formatted as `owner/repository`.
- Produces: `repository_https_url <slug>`, which prints the canonical HTTPS URL or returns nonzero; `sync_repositories <repos-file> <repos-dir>`, which clones through `gh` and refreshes through Git.

- [ ] **Step 1: Replace the local URL fixture with a slug and fake GitHub CLI**

Create `fake_bin`, `gh_log`, and a fake `gh` before the first sync invocation. The fake must accept `auth setup-git --hostname github.com --force` and translate:

```bash
gh repo clone https://github.com/test/example.git TARGET -- --recurse-submodules
```

into:

```bash
git clone --recurse-submodules https://github.com/test/example.git TARGET
```

Set `GIT_CONFIG_GLOBAL` to a temporary config containing:

```ini
[url "file:///ABSOLUTE/PATH/example.git"]
    insteadOf = https://github.com/test/example.git
```

Change `repo_list` to `test/example`, run all inner sync calls with the fake `gh` on `PATH`, and assert:

```bash
grep -Fxq 'auth setup-git --hostname github.com --force' "$gh_log"
grep -Eq '^repo clone https://github.com/test/example\.git .*/workspace/example -- --recurse-submodules$' "$gh_log"
[[ $(git -C "$workspace/example" config --get remote.origin.url) == \
    https://github.com/test/example.git ]]
```

Add a failing invalid-slug case for `test/example/extra`, while preserving duplicate, symlink, redirected-worktree, refresh, prune, and clean assertions.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
bash test-sync-workspace.sh
```

Expected: failure because the current implementation treats the slug as a Git URL and never invokes `gh`.

- [ ] **Step 3: Implement slug validation and proxied GitHub cloning**

Add:

```bash
repository_https_url() {
    local slug=$1
    if [[ ! $slug =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]]; then
        printf 'invalid GitHub repository slug: %s\n' "$slug" >&2
        return 1
    fi
    printf 'https://github.com/%s.git\n' "$slug"
}
```

Update both validation and synchronization passes to derive `url` with `repository_https_url`, derive `name` from the slug's final component, and detect duplicate destination names before creating the repository root.

After the first validation pass and before creating the repository root, run:

```bash
gh auth setup-git --hostname github.com --force
git config --global url.https://github.com/.insteadOf git@github.com:
git config --global --add url.https://github.com/.insteadOf ssh://git@github.com/
```

Clone missing repositories with:

```bash
gh repo clone "$url" "$target" -- --recurse-submodules
```

Keep the existing refresh/reset/clean/submodule operations, but set `origin` to the canonical HTTPS URL before fetching.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run:

```bash
bash test-sync-workspace.sh
```

Expected: all existing tests plus the new slug, `gh`, HTTPS-origin, and invalid-slug assertions pass.

- [ ] **Step 5: Commit repository synchronization**

```bash
git add sync-workspace.sh test-sync-workspace.sh
git commit -m "feat: sync GitHub repositories through gh"
```

---

### Task 2: Sandbox profile and DNS-ready instance lifecycle

**Files:**
- Modify: `test-sync-workspace.sh:104-210`
- Modify: `sync-workspace.sh:89-195`

**Interfaces:**
- Consumes: `profiles/sandbox.yaml`, the proxy CA initializer `go run ./proxy -init-ca`, and Incus profiles `default` and `sandbox`.
- Produces: `wait_for_dns`, a bounded readiness check against `github.com`; a temporary instance whose workspace and proxy configuration come from the sandbox profile.

- [ ] **Step 1: Extend the fake lifecycle test for profile, CA, and DNS behavior**

Add fake `go` logging and teach fake `incus` to:

- Record `profile show/create/edit sandbox`.
- Fail `exec INSTANCE -- getent ahosts github.com` for `FAKE_DNS_FAILURES` attempts, persisting the count in a temporary state file.
- Always accept `exec INSTANCE -- update-ca-certificates` after DNS succeeds.

For the success path, set `FAKE_DNS_FAILURES=1` and assert the ordered lifecycle includes:

```text
go run ./proxy -init-ca
profile edit sandbox
init test-image INSTANCE --profile default --profile sandbox
config device override INSTANCE workspace pool=default source=agent-workspace-seed path=/workspace
exec INSTANCE -- getent ahosts github.com
exec INSTANCE -- update-ca-certificates
```

Assert profile initialization and CA update occur before the inner sync `incus exec`. Replace the old `config device add` assertion with `config device override`.

Add a timeout case using `INCUS_DNS_TIMEOUT=1` and persistent DNS failure. Assert the script fails, reports `Timed out waiting for DNS`, deletes its instance, and never runs the inner sync command.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
bash test-sync-workspace.sh
```

Expected: lifecycle assertions fail because the script does not initialize the sandbox profile, wait for DNS, or update CA trust.

- [ ] **Step 3: Implement profile preparation and DNS readiness**

Add `profile_name=sandbox`, `profile_file="$script_dir/profiles/sandbox.yaml"`, and `dns_timeout=${INCUS_DNS_TIMEOUT:-60}`. Require `go`, `timeout`, and the profile file.

Before instance initialization:

```bash
(
    cd "$script_dir"
    go run ./proxy -init-ca
)
if incus profile show "$profile_name" >/dev/null 2>&1; then
    printf 'Refreshing Incus profile %s...\n' "$profile_name"
else
    printf 'Creating Incus profile %s...\n' "$profile_name"
    incus profile create "$profile_name"
fi
incus profile edit "$profile_name" < "$profile_file"
```

Initialize and override the inherited workspace device with:

```bash
incus init "$image" "$instance" --profile default --profile "$profile_name"
incus config device override "$instance" workspace \
    pool="$pool" source="$volume" path="$workspace_path"
```

Implement:

```bash
wait_for_dns() {
    local deadline=$((SECONDS + dns_timeout))
    local remaining
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        if timeout "${remaining}s" incus exec "$instance" -- \
            getent ahosts github.com >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    printf 'Timed out waiting for DNS in %s.\n' "$instance" >&2
    return 1
}
```

After `incus start`, call `wait_for_dns`, then `incus exec "$instance" -- update-ca-certificates`, and only then prepare files and invoke the inner sync.

- [ ] **Step 4: Run lifecycle tests and static checks**

Run:

```bash
bash test-sync-workspace.sh
shellcheck sync-workspace.sh test-sync-workspace.sh
git diff --check
```

Expected: all commands pass.

- [ ] **Step 5: Commit instance lifecycle support**

```bash
git add sync-workspace.sh test-sync-workspace.sh
git commit -m "fix: wait for proxied workspace networking"
```

---

### Task 3: Migrate configured repositories and verify end to end

**Files:**
- Modify: `private/repos.txt:1-14`

**Interfaces:**
- Consumes: the `owner/repository` format implemented in Task 1.
- Produces: the production repository list consumed by `sync-workspace.sh`.

- [ ] **Step 1: Convert the repository list to slugs**

Replace each `git@github.com:OWNER/REPOSITORY.git` line with `OWNER/REPOSITORY`, preserving order and all 14 repositories.

- [ ] **Step 2: Run the full automated verification suite**

Run:

```bash
bash test-sync-workspace.sh
bash test-launch-sandbox.sh
go test ./...
shellcheck sync-workspace.sh test-sync-workspace.sh launch-sandbox.sh
git diff --check
git status --short
```

Expected: all tests and static checks pass; `git status --short` shows only intended task files plus the pre-existing unstaged `build-image.sh` change.

- [ ] **Step 3: Perform the live sync verification**

Ensure the externally managed proxy is listening on `10.75.177.1:3128`, then run:

```bash
./sync-workspace.sh sandbox
```

Expected: DNS readiness succeeds, each repository clones or refreshes over HTTPS, the temporary instance is deleted, and `default/agent-workspace-seed` remains populated. If the external proxy is unavailable, report that environmental blocker without changing proxy supervision scope.

- [ ] **Step 4: Commit the production repository migration**

```bash
git add private/repos.txt
git commit -m "chore: use GitHub repository slugs"
```
