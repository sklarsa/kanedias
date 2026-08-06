# Live Incus Exec and Readiness Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Incus exec report guest exit status and flushed output, retry transient systemd startup failures, and order workspace initialization behind systemd readiness.

**Architecture:** Extend the existing thin Incus exec adapter with operation metadata and stream-completion handling. Keep condition-based readiness as focused private helpers in the sandbox and workspace packages, using each package's existing client seam and fixed 60-second bound.

**Tech Stack:** Go 1.26.5, github.com/lxc/incus/v7 v7.3.0, existing standard-library test fakes.

## Global Constraints

- Base commit is `d7a7804abb3884fe657c6495e76bfb900dbca8b1` in the isolated managed worktree.
- Production code must never invoke the `incus` executable; use the Incus Go client exclusively.
- Do not inspect or modify the concurrent `fix/agent-friendly-github-proxy` worktree or shared checkout.
- Do not change Cobra shape, configuration, profiles, dependencies, DNS timeout, cleanup timeout, volume naming, or repository behavior beyond the approved fixes.
- Exec must preserve stdout/stderr on every post-start error, honor `metadata["return"]`, wait for `DataDone`, and remain request-context cancellable.
- Systemd readiness accepts only `running` or `degraded`, uses condition-based retries, and retains the existing 60-second timeout.
- Workspace must wait for systemd before CA update, then retain bounded DNS waiting.
- Live commands use only `go run . --config /home/steven/source/github/kanedias/config.toml ...`; never print the config or secrets.

---

### Task 1: Record Approved Design and Establish Baseline

**Files:**
- Create: `docs/superpowers/specs/2026-08-06-live-incus-exec-readiness-fixes-design.md`
- Create: `docs/superpowers/plans/2026-08-06-live-incus-exec-readiness-fixes.md`

**Interfaces:** Documentation only.

- [ ] **Step 1: Self-review the design against the approved requirements**

Confirm the design covers exit metadata, `DataDone`, cancellation, malformed metadata, sandbox retry, workspace ordering, unchanged DNS bounds, and scope exclusions.

- [ ] **Step 2: Run the clean baseline**

Run:

```bash
git status --short --untracked-files=all
go test -count=1 ./...
```

Expected: only the two new documentation files are untracked and every baseline Go test passes.

- [ ] **Step 3: Commit approved documentation**

```bash
git add docs/superpowers/specs/2026-08-06-live-incus-exec-readiness-fixes-design.md docs/superpowers/plans/2026-08-06-live-incus-exec-readiness-fixes.md
git commit -m "docs: plan live Incus lifecycle fixes"
```

### Task 2: Honor Incus Exec Completion and Exit Status

**Files:**
- Modify: `internal/incusclient/client.go`
- Modify: `internal/incusclient/client_test.go`
- Modify: `internal/incusclient/instance.go`
- Modify: `internal/incusclient/instance_test.go`

**Interfaces:**
- Private `operationWaiter` adds `Get() api.Operation` alongside `WaitContext(context.Context) error`.
- Public `Client.Exec` signature remains unchanged.

- [ ] **Step 1: Add failing adapter tests**

Add tests where the fake operation:

- returns `Metadata: map[string]any{"return": float64(23)}` and requires an error containing `exit status 23` while stdout/stderr remain available;
- writes output only immediately before closing the supplied `DataDone` channel and requires `exec` not to return before that output is captured;
- omits `return` metadata or supplies a string/non-integral number and requires a clear metadata error with preserved output;
- leaves `DataDone` open until the context is cancelled and requires `context.Canceled` with preserved output.

Update existing success fakes to return `float64(0)` metadata and close `DataDone`.

- [ ] **Step 2: Run RED**

```bash
go test -count=1 ./internal/incusclient
```

Expected: FAIL because `operationWaiter` cannot expose metadata, `DataDone` is nil/not awaited, and non-zero return metadata is ignored.

- [ ] **Step 3: Implement minimal adapter behavior**

Create `DataDone: make(chan bool)` in `InstanceExecArgs`. After `WaitContext(ctx)`, wait for `DataDone` with:

```go
select {
case <-args.DataDone:
case <-ctx.Done():
    return stdoutBuffer.String(), stderrBuffer.String(), ctx.Err()
}
```

Read `operation.Get().Metadata["return"]`, require a finite integral `float64` that fits `int`, and return `fmt.Errorf("command in Incus instance exited with status %d", status)` when non-zero. Preserve buffers for operation, flush, metadata, and status errors.

- [ ] **Step 4: Run GREEN and commit**

```bash
gofmt -w internal/incusclient/*.go
go test -count=1 ./internal/incusclient
git add internal/incusclient
git commit -m "fix: honor Incus exec exit status"
```

Expected: adapter tests pass.

### Task 3: Retry Sandbox Systemd Readiness by Condition

**Files:**
- Modify: `internal/sandbox/sandbox.go`
- Modify: `internal/sandbox/sandbox_test.go`

**Interfaces:**
- Add private `waitForSystemd(context.Context, lifecycleClient, string, time.Duration, time.Duration) error` (timeout and poll interval are injected by the existing dependency struct).

- [ ] **Step 1: Add failing readiness tests**

Extend the recording fake with queued systemd responses. Add tests proving:

- the first response is empty with `Failed to connect to system scope bus via local transport: No such file or directory`, then `running`, and create succeeds after two readiness calls;
- the same pre-bus failure followed by `degraded` succeeds;
- repeated failures stop at a short injected timeout with `context.DeadlineExceeded` in the error chain;
- cancellation during retries stops promptly with `context.Canceled` in the error chain.

Use a small injected poll interval and condition/event channels for cancellation rather than fixed test sleeps.

- [ ] **Step 2: Run RED**

```bash
go test -count=1 ./internal/sandbox
```

Expected: FAIL because create makes only one systemd exec attempt.

- [ ] **Step 3: Implement minimal condition loop**

Under `context.WithTimeout(ctx, readinessTimeout)`, repeatedly execute `systemctl is-system-running --wait`. Return nil for trimmed `running` or `degraded`; otherwise retain the latest state/stderr/error, wait on a timer versus the readiness context, then check again. On deadline/cancellation, wrap `readyCtx.Err()` and include the last observation.

- [ ] **Step 4: Run GREEN and commit**

```bash
gofmt -w internal/sandbox/*.go
go test -count=1 ./internal/sandbox
git add internal/sandbox
git commit -m "fix: retry sandbox systemd readiness"
```

Expected: sandbox tests pass without fixed attempt counts.

### Task 4: Gate Workspace CA and DNS on Systemd Readiness

**Files:**
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`

**Interfaces:**
- Extend private workspace dependencies with readiness and DNS timeout/poll durations for fast deterministic tests; production defaults remain 60 seconds and one-second polling.
- Add private workspace `waitForSystemd` with the existing `client` seam.

- [ ] **Step 1: Add failing workspace ordering/retry tests**

Update the fake to queue a pre-bus systemd failure followed by `running`. Require this order:

```text
start
exec systemctl is-system-running --wait
exec systemctl is-system-running --wait
exec update-ca-certificates
exec getent ahosts github.com
```

Add a test where readiness never succeeds under a short timeout and assert neither CA update nor DNS nor repository setup occurs. Keep the existing assertion that the DNS exec context has a deadline no later than the configured DNS timeout.

- [ ] **Step 2: Run RED**

```bash
go test -count=1 ./internal/workspace
```

Expected: FAIL because workspace currently performs DNS before CA update and has no systemd readiness check.

- [ ] **Step 3: Implement minimal ordering and readiness loop**

After `StartInstance`, call the private condition-based readiness helper. On success run `update-ca-certificates`, then call the existing DNS loop using the injected fixed production bounds, and then proceed to repository setup.

- [ ] **Step 4: Run GREEN and commit**

```bash
gofmt -w internal/workspace/*.go
go test -count=1 ./internal/workspace
git add internal/workspace
git commit -m "fix: wait for workspace systemd readiness"
```

Expected: workspace tests pass and cleanup remains bounded/non-cancelled on failures.

### Task 5: Full Non-Live Verification

**Files:** No new files expected.

- [ ] **Step 1: Format and run focused tests**

```bash
gofmt -w internal/incusclient/*.go internal/sandbox/*.go internal/workspace/*.go
go test -count=1 ./internal/incusclient ./internal/sandbox ./internal/workspace
```

- [ ] **Step 2: Run complete verification**

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./...
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 3: Prove production does not invoke Incus CLI**

```bash
if rg -n 'exec\.Command(Context)?\([^\n]*"incus"|CommandContext\([^\n]*"incus"' --glob '*.go' --glob '!**/*_test.go' .; then exit 1; fi
```

Expected: no matches and exit zero.

### Task 6: Bounded Live Rerun and Cleanup Proof

**Files:**
- Temporary only: `/tmp/kanedias-live-*.log`
- Temporary Go probe outside tracked source or removed before completion.

- [ ] **Step 1: Choose a unique sandbox name and run live lifecycle commands**

Use `kanedias-live-<UTC timestamp>` and generous `timeout` bounds. Tee stdout/stderr to separate `/tmp` logs without reading or printing config:

```bash
timeout 45m go run . --config /home/steven/source/github/kanedias/config.toml image create 2>&1 | tee /tmp/kanedias-live-image.log
timeout 45m go run . --config /home/steven/source/github/kanedias/config.toml workspace sync 2>&1 | tee /tmp/kanedias-live-workspace.log
timeout 10m go run . --config /home/steven/source/github/kanedias/config.toml sandbox create "$name" 2>&1 | tee /tmp/kanedias-live-sandbox-create.log
timeout 10m go run . --config /home/steven/source/github/kanedias/config.toml sandbox destroy "$name" 2>&1 | tee /tmp/kanedias-live-sandbox-destroy.log
timeout 10m go run . --config /home/steven/source/github/kanedias/config.toml sandbox destroy "$name" 2>&1 | tee /tmp/kanedias-live-sandbox-destroy-idempotent.log
```

Stop dependent stages after a failure and resume systematic debugging with a failing test before any production change.

- [ ] **Step 2: Verify daemon cleanup through a Go-client-only probe**

Write a temporary Go program using `incus.ConnectIncusUnixWithContext` to list instance names, image aliases, storage pools, and custom volume names. Assert:

- the unique sandbox instance is absent;
- `kanedias-workspace-<unique-name>` is absent;
- no instance starts with `image-build-` or `workspace-sync-`;
- the configured seed volume still exists;
- image alias `sandbox` still exists.

Do not invoke the `incus` CLI. Remove the probe immediately afterward.

- [ ] **Step 3: Final cleanliness and commit check**

```bash
git status --short --untracked-files=all
git diff --check
git log --oneline --decorate -8
```

Expected: worktree is clean, all planned logical commits are present, and no temporary probe is tracked.

## Plan Self-Review

- Spec coverage: all approved adapter, sandbox, workspace, verification, commit, live rerun, and cleanup requirements have concrete tasks.
- Placeholder scan: no TBD/TODO or unspecified implementation steps remain.
- Type consistency: `operationWaiter.Get() api.Operation`, existing `Client.Exec`, and existing workflow client seams match current code.
- Scope: no new dependency, public API, Cobra/config/profile change, or cross-workflow framework is introduced.
