# Kanedias Repository Rename Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Kanedias the sole tracked project identity and replace the existing `main` history with one validated root commit.

**Architecture:** Apply a deterministic case-preserving byte replacement only to paths returned by `git ls-files`. Validate the renamed tree, obtain an independent review, then create a parentless commit from the staged tree with Git plumbing and repoint `main` to it.

**Tech Stack:** Git, Python 3, Go 1.26.5, Bash

## Global Constraints

- Rename every tracked occurrence of the retired identity while preserving lower-, title-, and upper-case forms.
- Leave `.git` internals and ignored `.pi-subagents/artifacts` unchanged.
- Keep the GitHub owner `sklarsa` and use `kanedias` as the repository/module name.
- Leave the repository without a configured remote.
- Finish with exactly one commit on `main`.

---

### Task 1: Rename the tracked repository and replace its history

**Files:**
- Modify: `build-image.sh`
- Modify: `docs/superpowers/plans/2026-08-05-proxy-observability.md`
- Modify: `docs/superpowers/specs/2026-08-05-proxy-observability-design.md`
- Modify: `docs/superpowers/specs/2026-08-05-sandbox-launch-scaffolding-design.md`
- Modify: `go.mod`
- Modify: `install.sh`
- Modify: `launch-sandbox.sh`
- Modify: `profiles/sandbox.yaml`
- Modify: `proxy/incus_test.go`
- Modify: `proxy/live_incus_test.go`
- Modify: `proxy/main.go`
- Modify: `proxy/oauth_test.go`
- Modify: `proxy/observability.go`
- Modify: `proxy/observability_test.go`
- Modify: `remove_sandbox.sh`
- Modify: `sync-workspace.sh`
- Modify: `test-install.sh`
- Modify: `test-launch-sandbox.sh`
- Modify: `test-remove-sandbox.sh`
- Verify: `proxy/*_test.go`
- Verify: `test-launch-sandbox.sh`
- Verify: `test-remove-sandbox.sh`
- Verify: `test-sync-workspace.sh`

**Interfaces:**
- Consumes: the clean tracked tree on `main` and Git's tracked-file list
- Produces: a parentless `main` commit whose tree uses Kanedias consistently

- [x] **Step 1: Record a recoverable pre-rewrite reference and baseline state**

```bash
test -z "$(git status --porcelain)"
test -z "$(git remote)"
git update-ref refs/rename-backup HEAD
git rev-parse refs/rename-backup
```

Expected: the worktree is clean, no remotes are printed, and Git prints the current commit ID stored in `refs/rename-backup`.

- [x] **Step 2: Apply the case-preserving replacement to tracked files**

```bash
python3 - <<'PY'
from pathlib import Path
import subprocess

replacements = (
    (("SEM" + "UTA").encode(), b"KANEDIAS"),
    (("Sem" + "uta").encode(), b"Kanedias"),
    (("sem" + "uta").encode(), b"kanedias"),
)

raw_paths = subprocess.check_output(("git", "ls-files", "-z"))
for raw_path in raw_paths.split(b"\0"):
    if not raw_path:
        continue
    path = Path(raw_path.decode())
    original = path.read_bytes()
    updated = original
    for old, new in replacements:
        updated = updated.replace(old, new)
    if updated != original:
        path.write_bytes(updated)
PY
```

Expected: only tracked files with identity references are modified.

- [x] **Step 3: Verify replacement coverage and patch integrity**

```bash
old_identity=$(printf 'sem%s' 'uta')
if git grep -Iin -- "$old_identity"; then
    echo "retired identity remains in tracked files" >&2
    exit 1
fi
git diff --check
git diff --stat
```

Expected: the search prints no matches, `git diff --check` exits successfully, and the stat lists the renamed project files.

- [x] **Step 4: Run formatting, syntax, and automated tests**

```bash
test -z "$(gofmt -l proxy)"
git ls-files -z '*.sh' | xargs -0 -n1 bash -n
go test ./...
./test-launch-sandbox.sh
./test-remove-sandbox.sh
./test-sync-workspace.sh
```

Expected: Go formatting is unchanged, every shell script parses, all Go tests pass, and each self-contained shell suite prints `PASS`.

`test-install.sh` is a live Incus provisioning test rather than a self-contained suite. Run it only when a usable Incus daemon and the required network access are available:

```bash
if command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1; then
    ./test-install.sh
else
    echo 'SKIP: live Incus install test unavailable'
fi
```

- [x] **Step 5: Create an interim review commit**

```bash
git add -A
git diff --cached --check
git commit -m "rename project to kanedias"
```

Expected: Git creates one commit containing the complete tracked rename and its approved design/plan documents.

- [x] **Step 6: Obtain independent review and resolve findings**

Request review of `refs/rename-backup..HEAD` against `docs/superpowers/specs/2026-08-06-kanedias-rename-design.md`. Resolve all Critical and Important findings, rerun Step 4, and amend the interim commit:

```bash
git add -A
git diff --cached --check
git commit --amend --no-edit
```

Expected: the reviewer confirms the tracked rename is complete and no important finding remains unresolved.

- [x] **Step 7: Build and install the single root commit**

```bash
git add -A
root_tree=$(git write-tree)
root_commit=$(printf '%s\n' 'Initial Kanedias repository' | git commit-tree "$root_tree")
git reset --hard "$root_commit"
```

Expected: `main` points to a newly created commit with no parent and the fully renamed tree.

- [x] **Step 8: Verify the final repository before removing the backup reference**

```bash
old_identity=$(printf 'sem%s' 'uta')
test "$(git rev-list --count main)" -eq 1
test "$(git rev-list --max-parents=0 --count main)" -eq 1
test -z "$(git status --porcelain)"
test -z "$(git remote)"
if git grep -Iin -- "$old_identity"; then
    echo "retired identity remains in tracked files" >&2
    exit 1
fi
test -z "$(gofmt -l proxy)"
git ls-files -z '*.sh' | xargs -0 -n1 bash -n
go test ./...
./test-launch-sandbox.sh
./test-remove-sandbox.sh
./test-sync-workspace.sh
git log --oneline --decorate --graph --all
```

Expected: every check passes; the log shows the new root commit on `main` and the old line only through `refs/rename-backup`.

- [x] **Step 9: Remove the recovery reference and perform the final audit**

```bash
git update-ref -d refs/rename-backup
test "$(git rev-list --count --all)" -eq 1
test -z "$(git status --porcelain)"
test -z "$(git remote)"
git log --oneline --decorate --graph --all
```

Expected: exactly one reachable commit remains, `main` is clean, no remote exists, and the final log contains only `Initial Kanedias repository`.
