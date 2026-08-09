# Container Lifecycle Housecleaning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the manual sandbox and incomplete nested-Incus paths so supervisor-managed sessions are the only supported container lifecycle.

**Architecture:** Delete both obsolete CLI entry points and their implementation packages, then remove the configuration, image setup, profile privileges, helpers, tests, and historical documents that existed only for nested Incus. Preserve the supervisor provisioners, repository workspace synchronization, and general container nesting needed by Docker/kind. Keep GitHub issue #21 open as a precise restoration plan rather than carrying half-integrated code.

**Tech Stack:** Go 1.26, Cobra, Incus v7 client, TOML, Bash, GitHub CLI.

## Global Constraints

- Supervisor-managed root and child session provisioning remains the only container launch path.
- `workspace repos sync` remains supported.
- The `sandbox` profile remains because supervisor-managed sessions use it.
- `security.nesting: "true"` and `security.privileged: "false"` remain for general unprivileged container workloads.
- No destructive live Incus test runs without its explicit environment authorization.
- Remove all `docs/superpowers/**` files from the final tree, including this temporary plan and its design.
- Preserve shared Incus helpers that remain referenced by supervisor provisioning.

---

### Task 1: Remove the manual sandbox and nested-workspace CLI surfaces

**Files:**
- Modify: `cmd/root_test.go`
- Modify: `cmd/root.go`
- Modify: `cmd/workspace.go`
- Delete: `cmd/sandbox.go`
- Delete: `internal/sandbox/lock.go`
- Delete: `internal/sandbox/sandbox.go`
- Delete: `internal/sandbox/sandbox_test.go`

**Interfaces:**
- Consumes: existing `services.syncRepos func(context.Context, config.Config, io.Writer, io.Writer) error`.
- Produces: a root command without `sandbox`; a `workspace` parent whose only child is `repos`; a `services` struct without manual sandbox or nested-workspace callbacks.

- [ ] **Step 1: Change the CLI hierarchy test to the desired single lifecycle**

In `TestCommandHierarchyAndFlags`, make the expected hierarchy exactly:

```go
assertChildCommands(t, root, "image", "profile", "proxy", "server", "session", "workspace")
assertChildCommands(t, mustFindCommand(t, root, "workspace"), "repos")
assertChildCommands(t, mustFindCommand(t, root, "workspace", "repos"), "sync")
```

Remove `sandbox`, `sandbox create`, `sandbox destroy`, and `workspace incus sync` from all command-path and flag tables. Remove their cases and service stubs from `TestLifecycleCommandDelegation`, `TestLifecycleCommandsStopWhenConfigLoadFails`, `TestLifecycleCommandsRejectExtraArguments`, `TestWorkspaceParentShowsHelpAndRejectsLegacySync`, `stubServices`, and `serverServicesThatRejectDependencies`.

- [ ] **Step 2: Run the focused CLI test and confirm the incomplete deletion is red**

Run:

```bash
go test ./cmd -run 'TestCommandHierarchyAndFlags|TestWorkspaceParentShowsHelpAndRejectsLegacySync|TestLifecycleCommand' -count=1
```

Expected: FAIL to compile because `cmd/root.go` still imports `internal/sandbox` and still wires removed services/commands.

- [ ] **Step 3: Remove obsolete CLI production wiring**

In `cmd/root.go`:

- remove imports of `internal/sandbox` and `internal/workspace/incus`;
- remove `createSandbox`, `destroySandbox`, and `syncIncusWorkspace` from `services`;
- remove their assignments from `realServices`;
- remove `newSandboxCommand(...)` from `root.AddCommand(...)`.

In `cmd/workspace.go`, make the child list:

```go
command.AddCommand(newWorkspaceReposCommand(service, configPath))
```

Delete `newWorkspaceIncusCommand` and `newWorkspaceIncusSyncCommand`. Preserve the four already requested deletions listed in this task.

- [ ] **Step 4: Format and run CLI tests**

Run:

```bash
gofmt -w cmd/root.go cmd/root_test.go cmd/workspace.go
go test ./cmd -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the coherent CLI/package removal**

```bash
git add cmd/root.go cmd/root_test.go cmd/workspace.go cmd/sandbox.go internal/sandbox
git commit -m "refactor: remove manual sandbox lifecycle"
```

---

### Task 2: Remove the nested-Incus workspace implementation and configuration

**Files:**
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/config.go`
- Modify: `config.toml`
- Modify: `.env.example`
- Delete: `internal/workspace/incus/inner.go`
- Delete: `internal/workspace/incus/inner_test.go`
- Delete: `internal/workspace/incus/live_incus_test.go`
- Delete: `internal/workspace/incus/lock.go`
- Delete: `internal/workspace/incus/lock_test.go`
- Delete: `internal/workspace/incus/state.go`
- Delete: `internal/workspace/incus/state_test.go`
- Delete: `internal/workspace/incus/sync.go`
- Delete: `internal/workspace/incus/sync_test.go`
- Modify: `internal/incusclient/client.go`
- Modify: `internal/incusclient/storage.go`
- Modify: `internal/incusclient/storage_test.go`
- Modify: `internal/incusclient/names.go`

**Interfaces:**
- Consumes: `config.Workspace` fields `Pool`, `Volume`, and `Repos`; `incusclient.Client.CopyStorageVolume` for supervisor COW provisioning.
- Produces: configuration with no `IncusWorkspace`; no nested seed or state package; no terminal-wait copy helper used only by nested seed locking.

- [ ] **Step 1: Change configuration tests to describe the desired schema**

In `internal/config/config_test.go`, remove `[workspace.incus]` fixture blocks and all expectations involving:

```go
IncusWorkspace
Workspace.Incus
DefaultIncusWorkspaceVolume
```

Remove the lifecycle-validation case that exists only to reject equal repository and nested-Incus seed volumes. Keep all assertions for `Workspace.Pool`, `Workspace.Volume`, and `Workspace.Repos`.

- [ ] **Step 2: Run configuration tests and verify the stale fields remain visible**

Run:

```bash
go test ./internal/config -count=1
```

Expected: PASS for the adjusted behavior tests, while the production schema still contains now-unreferenced nested-Incus fields. Confirm the stale surface with:

```bash
git grep -n 'IncusWorkspace\|DefaultIncusWorkspaceVolume\|Workspace\.Incus' -- internal/config config.toml
```

Expected: matches in production code/config, proving implementation removal remains.

- [ ] **Step 3: Remove nested workspace config and package code**

Change `Workspace` in `internal/config/config.go` to:

```go
type Workspace struct {
	Pool   string   `toml:"pool"`
	Volume string   `toml:"volume"`
	Repos  []string `toml:"repos"`
}
```

Remove `IncusWorkspace`, `DefaultIncusWorkspaceVolume`, its defaulting in `Load`, and the distinct repository/nested seed check from `ValidateLifecycle`. Remove `[workspace.incus]` from `config.toml` and `KANEDIAS_LIVE_NESTED_INCUS` from `.env.example`. Delete `internal/workspace/incus/**`.

- [ ] **Step 4: Remove the nested-only storage-copy strategy**

In `internal/incusclient/storage.go`, delete only:

```go
// CopyStorageVolumeUntilTerminal cancels the remote target on caller
// cancellation but does not return until the submitted copy is terminal.
func (c *Client) CopyStorageVolumeUntilTerminal(ctx context.Context, pool, source, target string) error {
	return c.copyStorageVolume(ctx, pool, source, target, submitAndWaitRemoteOperationUntilTerminal)
}
```

Delete `TestCopyStorageVolumeUntilTerminalUsesAdapterAndHoldsCancellationUntilRemoteTerminal` from `internal/incusclient/storage_test.go`. In `internal/incusclient/client.go`, also delete the now-unreferenced `submitAndWaitRemoteOperationUntilTerminal` and `waitRemoteOperationUntilTerminal` functions; retain `submitAndWaitRemoteOperation`, `submitAndWaitRemote`, and `waitSubmittedRemoteOperation`. Keep `CopyStorageVolume`, `OperationWasSubmitted`, `AwaitSubmittedOperation`, and their supervisor-facing tests. Update `internal/incusclient/names.go` so its comment says the validator is the source of truth for supervisor child instance and volume names, without mentioning the removed sandbox package.

- [ ] **Step 5: Format and validate configuration, client, and repository packages**

Run:

```bash
gofmt -w internal/config/config.go internal/config/config_test.go internal/incusclient/client.go internal/incusclient/storage.go internal/incusclient/storage_test.go internal/incusclient/names.go
go test ./internal/config ./internal/incusclient ./internal/workspace/... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the nested workspace removal**

```bash
git add .env.example config.toml internal/config internal/incusclient internal/workspace/incus
git commit -m "refactor: remove nested Incus workspace"
```

---

### Task 3: Remove nested-Incus image and profile prerequisites

**Files:**
- Modify: `internal/image/image_test.go`
- Modify: `internal/image/install.sh`
- Modify: `internal/profiles/profiles_test.go`
- Modify: `internal/profiles/sandbox.yaml`

**Interfaces:**
- Consumes: the existing base-image installer and `profiles.Render`.
- Produces: a session image without an inner Incus daemon/client setup; a sandbox profile that remains unprivileged and supports general nesting but has no nested-Incus syscall intercepts.

- [ ] **Step 1: Replace the installer expectation with an exclusion test**

Replace `TestInstallerIncludesUninitializedContainerOnlyIncus` with:

```go
func TestInstallerExcludesNestedIncus(t *testing.T) {
	script := string(installer)
	for _, forbidden := range []string{"incus-base", "incus-admin", "incus admin init"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("installer contains nested Incus setup %q", forbidden)
		}
	}
}
```

Remove the now-unused `slices` import if no other test uses it.

- [ ] **Step 2: Change the profile test to preserve general nesting and reject nested-only intercepts**

In `TestRenderSandboxUsesLifecycleDevicesAndDefaultProxyCA`, keep positive checks for:

```go
`  security.nesting: "true"`
`  security.privileged: "false"`
```

Add:

```go
for _, unwanted := range []string{
	"security.syscalls.intercept.mknod",
	"security.syscalls.intercept.setxattr",
} {
	if strings.Contains(rendered, unwanted) {
		t.Errorf("rendered sandbox retained nested Incus setting %q", unwanted)
	}
}
```

- [ ] **Step 3: Run focused tests and verify they fail against current setup**

Run:

```bash
go test ./internal/image ./internal/profiles -count=1
```

Expected: FAIL because `install.sh` still installs/configures Incus and `sandbox.yaml` still contains both syscall intercepts.

- [ ] **Step 4: Remove nested-Incus prerequisites**

From `internal/image/install.sh`, remove `incus-base` from the package list and remove `incus-admin` from the managed user's supplemental groups. Preserve the UID/GID 1000 assertion because supervisor proxy device mappings use it, but change its error text from `Incus proxy mappings` to `container device mappings`.

From `internal/profiles/sandbox.yaml`, remove only:

```yaml
security.syscalls.intercept.mknod: "true"
security.syscalls.intercept.setxattr: "true"
```

Keep `security.nesting` and `security.privileged` unchanged.

- [ ] **Step 5: Validate installer and profile behavior**

Run:

```bash
bash -n internal/image/install.sh
go test ./internal/image ./internal/profiles -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit image/profile cleanup**

```bash
git add internal/image/image_test.go internal/image/install.sh internal/profiles/profiles_test.go internal/profiles/sandbox.yaml
git commit -m "chore: remove nested Incus image setup"
```

---

### Task 4: Remove historical Superpowers documents and verify the complete cleanup

**Files:**
- Delete: `docs/superpowers/plans/2026-08-07-native-nested-incus-workspace.md`
- Delete: `docs/superpowers/plans/2026-08-07-recursive-pi-session-supervisor.md`
- Delete: `docs/superpowers/plans/2026-08-07-server-managed-supervisors.md`
- Delete: `docs/superpowers/plans/2026-08-09-container-housecleaning.md`
- Delete: `docs/superpowers/specs/2026-08-07-makefile-ci-design.md`
- Delete: `docs/superpowers/specs/2026-08-07-native-nested-incus-workspace-design.md`
- Delete: `docs/superpowers/specs/2026-08-07-server-managed-supervisors-design.md`
- Delete: `docs/superpowers/specs/2026-08-09-container-housecleaning-design.md`

**Interfaces:**
- Consumes: the code cleanup from Tasks 1–3.
- Produces: no repository-local Superpowers planning archive and no current nested-Incus production surface.

- [ ] **Step 1: Delete the complete historical planning directory**

Run:

```bash
rm -rf docs/superpowers
```

- [ ] **Step 2: Search for obsolete production references**

Run:

```bash
if git grep -n -E 'internal/sandbox|internal/workspace/incus|IncusWorkspace|DefaultIncusWorkspaceVolume|CopyStorageVolumeUntilTerminal|KANEDIAS_LIVE_NESTED_INCUS|workspace\.incus|incus-state|workspace incus sync' -- .; then
  exit 1
fi
if git grep -n -E 'incus-base|incus-admin|security\.syscalls\.intercept\.(mknod|setxattr)' -- internal config.toml .env.example; then
  exit 1
fi
```

Expected: both searches produce no matches and exit successfully. Do not treat ordinary words such as `sandbox`, Incus host-client code, or general `security.nesting` as stale: those remain part of supervisor-managed containers.

- [ ] **Step 3: Run repository verification**

Run:

```bash
gofmt -w cmd internal
make test
make build
go test -tags incus -run '^$' ./...
bash -n internal/image/install.sh
git diff --check
```

Expected: all commands PASS. The tagged command compiles Incus-gated tests without executing destructive test bodies.

If `golangci-lint` is installed, also run:

```bash
make lint
```

Expected: PASS. If unavailable, record that exact tooling gap in the PR validation section rather than installing an unpinned tool.

- [ ] **Step 4: Review the final diff for scope**

Run:

```bash
git status --short
git diff --stat origin/main
git diff origin/main -- cmd internal config.toml .env.example docs
```

Confirm that supervisor/session provisioning, `workspace repos sync`, the `sandbox` profile, and general nesting remain intact.

- [ ] **Step 5: Commit document removal and final cleanup**

```bash
git add -A docs/superpowers
git commit -m "docs: remove superpowers planning archive"
```

---

### Task 5: Update issue #21, open the pull request, and merge

**Files:**
- Create outside repository: `/tmp/kanedias-issue-21.md`
- No source-file changes expected.

**Interfaces:**
- Consumes: a clean, verified `rm-sandbox` branch and GitHub issue #21.
- Produces: an updated open restoration issue, a merged pull request against `main`, and a final URL/status receipt.

- [ ] **Step 1: Synchronize with the latest remote base**

Run:

```bash
git fetch origin
git rebase origin/main
make test
make build
```

Expected: rebase succeeds and both verification commands PASS. Resolve no semantic conflict by guessing; stop for user input if `main` changed container lifecycle behavior.

- [ ] **Step 2: Push and create the pull request**

Run:

```bash
git push -u origin rm-sandbox
```

Create a PR against `main` with a concise housecleaning summary and the exact verification commands/results. The body must state that supervisor-managed sessions are now the sole launch path, general nesting remains, and #21 tracks a future production-ready nested-Incus design.

- [ ] **Step 3: Rewrite issue #21 as the restoration handoff**

Set the title to:

```text
Restore production-ready nested Incus within the supervisor lifecycle
```

Write `/tmp/kanedias-issue-21.md` with these concrete sections:

```markdown
## Status
Nested Incus was removed by <PR URL> because it was a separate, incomplete manual sandbox path. This issue remains open as the implementation tracker.

## Where the proof of concept stopped
- The image installed `incus-base` but intentionally did not initialize Incus.
- `workspace incus sync` attempted to initialize a cold Btrfs state seed and preload images.
- `sandbox create` cloned that state and mounted it at `/var/lib/incus`.
- The production supervisor/session path never attached that state volume.
- Live initialization first failed because the image lacked `dnsmasq`.
- Nested instance startup then failed in the unprivileged outer container with BPF program-load and `newuidmap`/UID-map permission errors.
- Recursive Btrfs subvolume copy, backing-mount identity, and isolation were not exercised in CI.

## Required design decisions
- Integrate nested Incus into the single supervisor-owned root/child lifecycle; do not restore a second manual `sandbox` command.
- Decide and document the security model: privileged outer container, additional narrowly scoped capabilities, or a different nesting mechanism.
- Define state ownership, mount paths, snapshot/COW behavior, cleanup, locking, crash recovery, and disk-pressure limits.
- Decide how the host model proxy and credentials propagate through host → outer session → nested instance.

## Implementation steps
1. Build a minimal live reproducer for daemon initialization and nested instance launch on the supported host/Incus/Btrfs versions.
2. Add every required image package and service configuration, including networking/DNS dependencies, without initializing mutable state in the published image.
3. Prove the selected BPF/user-namespace security model boots an inner unprivileged instance; document every privilege granted.
4. Implement cold nested state creation and quiescing with explicit ownership and terminal-operation handling.
5. Attach or clone nested state through supervisor root/child provisioning, with no competing CLI lifecycle.
6. Verify the attached backing storage, recursive Btrfs snapshots, image availability, child isolation, cleanup, and crash recovery.
7. Propagate and test the local-model proxy into nested instances.
8. Add opt-in destructive live tests and a safe CI lane with explicit prerequisites and cleanup auditing.
9. Update architecture and operator documentation only after the live acceptance path passes.

## Acceptance criteria
- A supervisor-launched session starts an inner instance successfully on the supported production host.
- Two concurrent sessions receive isolated writable nested state backed by verified COW snapshots.
- Inner instances can reach the configured model and required network endpoints.
- Normal, failed, cancelled, and host-restart paths leave no leaked instances, mounts, volumes, locks, or credentials.
- Hermetic tests, tagged compilation, and authorized live tests all pass.
```

Apply it with:

```bash
gh issue edit 21 --title "Restore production-ready nested Incus within the supervisor lifecycle" --body-file /tmp/kanedias-issue-21.md
```

Expected: issue #21 remains OPEN and links the new PR.

- [ ] **Step 4: Wait for required PR checks and merge**

Run:

```bash
gh pr checks --watch <PR URL>
gh pr merge --squash --delete-branch <PR URL>
```

Expected: required checks pass and the PR state becomes MERGED. Do not bypass branch protection or force-push after a rejection.

- [ ] **Step 5: Capture final receipts**

Run:

```bash
gh pr view <PR URL> --json number,state,mergedAt,mergeCommit,url
gh issue view 21 --json number,title,state,url
```

Expected: PR state `MERGED`; issue #21 state `OPEN` with the restoration title. Report the PR URL, merge commit, issue URL, verification evidence, and any skipped unavailable lint tooling.
