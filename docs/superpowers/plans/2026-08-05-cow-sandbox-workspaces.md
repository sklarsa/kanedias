# COW Sandbox Workspaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every launched sandbox an isolated Btrfs COW clone of `agent-workspace-seed`.

**Architecture:** `launch-sandbox.sh` copies the seed before instance initialization, overrides the inherited workspace device before startup, and tracks ownership of the clone and instance independently. Failure cleanup removes only resources created by the current invocation, in dependency order.

**Tech Stack:** Bash, Incus 7.2, Btrfs custom filesystem volumes

## Global Constraints

- Source volume: `default/agent-workspace-seed`.
- Destination volume: `default/agent-workspace-<instance>`.
- Existing destination volumes are never reused or deleted.
- Failure cleanup deletes an owned instance before its owned volume.
- Successful instances and volumes remain persistent.

---

### Task 1: Test per-sandbox volume lifecycle

**Files:**
- Modify: `test-launch-sandbox.sh`

- [ ] Add fake Incus support for `storage volume copy`, `config device override`, and `storage volume delete` with independent instance/volume state.
- [ ] Assert default and custom volume names, `--volume-only`, and copy → init → override → start ordering.
- [ ] Assert CA-update/start/init failures delete all and only owned resources in instance-before-volume order.
- [ ] Assert a copy collision neither initializes an instance nor deletes the existing target volume.
- [ ] Run `bash test-launch-sandbox.sh` and confirm RED because the launcher does not yet copy or override workspaces.

### Task 2: Implement COW workspace ownership

**Files:**
- Modify: `launch-sandbox.sh`

- [ ] Add `workspace_pool=default`, `workspace_seed=agent-workspace-seed`, `workspace_volume=agent-workspace-$instance`, and a volume ownership flag.
- [ ] Copy the volume using `incus storage volume copy ... --volume-only` before `incus init`.
- [ ] Override the inherited `workspace` device to the cloned source before `incus start`.
- [ ] Extend the EXIT trap to delete an owned instance first, then an owned volume.
- [ ] Run the focused shell test and static checks until GREEN.

### Task 3: Verify real Btrfs isolation

**Files:**
- No production changes unless verification exposes a scoped defect.

- [ ] Launch a temporary sandbox through the real script.
- [ ] Verify its expanded workspace source is `agent-workspace-<instance>`.
- [ ] Write a marker inside its `/workspace` and verify the marker is absent from the seed volume.
- [ ] Verify the clone is a Btrfs snapshot/subvolume, then delete the temporary instance and cloned volume.
- [ ] Run `go test ./...`, `bash test-launch-sandbox.sh`, `bash test-sync-workspace.sh`, ShellCheck on changed scripts, and `git diff --check`.
