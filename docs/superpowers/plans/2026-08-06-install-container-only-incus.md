# Container-Only Incus Installation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Install Debian 13's container-only Incus daemon/client in the Kanedias image without initializing it.

**Architecture:** Extend the existing initial Debian package batch and managed-user group setup in `internal/image/install.sh`. Protect the embedded installer contract with one focused Go test, then rebuild and inspect the real image on local Incus.

**Tech Stack:** Bash, Debian 13 APT, Go embed/tests, Incus 7.3

## Global Constraints

- Install `incus-base`, not the VM-oriented `incus` metapackage.
- Use Debian 13's existing official repositories; add no key or package source.
- Add `kanedias` to `incus-admin`.
- Do not initialize Incus, create nested storage/network state, invoke the Incus client from the installer, or explicitly start its service.
- Do not modify existing Incus profiles or profile selection.
- Use an isolated worktree and merge the verified branch into `main` locally.

---

### Task 1: Add Container-Only Incus to the Embedded Installer

**Files:**
- Modify: `internal/image/install.sh`
- Modify: `internal/image/image_test.go`

**Interfaces:**
- Extends the embedded `installer []byte` artifact consumed by `image.Create`.
- Leaves all Go production interfaces unchanged.

- [ ] **Step 1: Add a failing embedded-installer test**

Add the `slices` import and this test to `internal/image/image_test.go`:

```go
func TestInstallerIncludesUninitializedContainerOnlyIncus(t *testing.T) {
    script := string(installer)
    const startMarker = "apt-get install -y --no-install-recommends \\\n"
    start := strings.Index(script, startMarker)
    if start < 0 {
        t.Fatal("installer initial package batch not found")
    }
    packageBlock := script[start+len(startMarker):]
    end := strings.Index(packageBlock, "\n\nrun_as_managed_user()")
    if end < 0 {
        t.Fatal("installer initial package batch terminator not found")
    }
    packages := strings.Fields(strings.ReplaceAll(packageBlock[:end], "\\\n", " "))

    if !slices.Contains(packages, "incus-base") {
        t.Error("initial package batch does not include incus-base")
    }
    if slices.Contains(packages, "incus") {
        t.Error("initial package batch includes VM-oriented incus metapackage")
    }
    if !strings.Contains(script, `usermod --append --groups sudo,incus-admin "$managed_user"`) {
        t.Error("managed user is not added to incus-admin")
    }
    if strings.Contains(script, "incus admin init") {
        t.Error("installer initializes Incus")
    }
}
```

Use only test-local parsing; add no production helper for the test.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test -count=1 ./internal/image -run TestInstallerIncludesUninitializedContainerOnlyIncus
```

Expected: FAIL because `incus-base` and `incus-admin` are absent.

- [ ] **Step 3: Implement the minimum installer change**

In the initial lexical package list, add:

```text
incus-base
```

Change the managed-user group command to:

```bash
usermod --append --groups sudo,incus-admin "$managed_user"
```

Do not add any Incus command or service operation.

- [ ] **Step 4: Verify GREEN and static shell validity**

Run:

```bash
go test -count=1 ./internal/image
shellcheck internal/image/install.sh
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 5: Commit**

```bash
git add internal/image/install.sh internal/image/image_test.go
git commit -m "feat: install container-only Incus"
```

---

### Task 2: Live Image and Runtime Validation

**Files:**
- Temporary host-side instance: `kanedias-incus-install-<timestamp>`

**Interfaces:**
- Consumes the existing `image create` workflow and `default + image-build` profiles.
- Produces a published `sandbox` image containing an uninitialized `incus-base` installation.

- [ ] **Step 1: Rebuild the image through the real workflow**

From the worktree, run:

```bash
timeout 45m go run . --config /home/steven/source/github/kanedias/config.toml image create
```

Expected: the installer completes, the image alias is published, and the temporary `image-build-*` instance is deleted.

- [ ] **Step 2: Launch a disposable validation container**

```bash
name="kanedias-incus-install-$(date -u +%Y%m%d%H%M%S)"
incus launch sandbox "$name" --profile default --profile image-build
```

Wait conditionally for `systemctl is-system-running --wait` to report `running` or `degraded`.

- [ ] **Step 3: Verify package, access, and uninitialized state**

Run:

```bash
incus exec "$name" -- dpkg-query -W -f='${Status}\n' incus-base
incus exec "$name" -- runuser -u kanedias -- id -nG
incus exec "$name" -- runuser -u kanedias -- incus version
incus exec "$name" -- runuser -u kanedias -- incus storage list --format csv -c n
```

Expected: `incus-base` is installed, groups include `incus-admin`, client/server versions print, and storage output is empty.

- [ ] **Step 4: Clean up and prove no temporary instance remains**

Run even on failure:

```bash
incus delete "$name" --force
! incus list --format csv -c n | grep -E '^(image-build-|kanedias-incus-install-)'
```

---

### Task 3: Final Verification, Lightweight Review, and Merge

**Files:** No additional files expected.

- [ ] **Step 1: Run final verification**

```bash
go test -count=1 ./...
go test -race -count=1 ./...
shellcheck internal/image/install.sh
go vet ./...
go build ./...
git diff --check
git status --short --branch
```

Expected: every check passes and the feature worktree is clean.

- [ ] **Step 2: Perform one focused read-only review**

Review only the installer/test diff for package correctness, absence of initialization, user access, and regression risk. Fix blockers only; avoid extra review rounds for optional polish.

- [ ] **Step 3: Merge locally and verify `main`**

Merge the feature branch into `main`, rerun:

```bash
go test -count=1 ./...
shellcheck internal/image/install.sh
git status --short --branch
```

Then remove the owned worktree and delete the merged feature branch.
