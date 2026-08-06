# Remove Sandbox Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Safely remove one launched sandbox and its deterministic COW workspace volume.

**Architecture:** A standalone Bash script derives the volume from the instance name, validates ownership through the instance-local workspace override, and deletes resources in dependency order. Missing resources are handled idempotently while mismatches fail closed.

**Tech Stack:** Bash, Incus CLI

## Global Constraints

- Interface: `remove_sandbox.sh <instance-name>`.
- Owned volume: `default/agent-workspace-<instance-name>`.
- Never delete an existing instance whose local workspace source does not exactly match the derived volume.
- Delete an existing instance before deleting its volume.
- Missing instance plus existing matching volume means orphan cleanup.
- Missing instance and volume is a successful no-op.
- Existence comes from successful structured CSV listings; lookup command failures stop removal.
- Launch and removal serialize same-name operations with a shared per-instance `flock`.

---

### Task 1: Specify removal behavior with failing tests

**Files:**
- Create: `test-remove-sandbox.sh`

- [ ] Build a stateful fake Incus CLI supporting structured instance/custom-volume lists, `config device get`, `delete --force`, and `storage volume delete`.
- [ ] Test normal instance-before-volume deletion, missing-device and mismatched-device refusal, failed instance deletion preserving the volume, orphan-volume cleanup, and idempotent missing-resource behavior.
- [ ] Test argument validation, exact deterministic volume naming, lookup failures, and lifecycle lock contention.
- [ ] Run `bash test-remove-sandbox.sh`; expect RED because `remove_sandbox.sh` is absent.

### Task 2: Implement safe removal

**Files:**
- Create: `remove_sandbox.sh`

- [ ] Validate exactly one non-empty argument and require Incus.
- [ ] Derive `agent-workspace-<instance>` without accepting a configurable seed name.
- [ ] Acquire the shared per-instance lifecycle lock and obtain successful CSV instance/volume listings.
- [ ] If the instance exists, read its local `workspace` source and fail unless it exactly matches the derived volume.
- [ ] Delete the instance and only then continue to volume deletion.
- [ ] Delete the volume when present; otherwise report an idempotent no-op.
- [ ] Run the focused test, Bash syntax check, ShellCheck, and `git diff --check` until GREEN.

### Task 3: Live lifecycle verification

**Files:**
- No production changes unless verification exposes a scoped defect.

- [ ] Launch a temporary sandbox through `launch-sandbox.sh`.
- [ ] Remove it through `remove_sandbox.sh`.
- [ ] Verify both the instance and `agent-workspace-<instance>` are absent while `agent-workspace-seed` remains.
- [ ] Run all Go, launcher, remover, workspace-sync, and static checks.
