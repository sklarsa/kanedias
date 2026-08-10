# Child Liveness Shutdown Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ensure acknowledged child supervisors exit promptly and ensure parents bound and reap any terminal child that wedges during final shutdown.

**Architecture:** Mark inherited liveness descriptor 4 nonblocking before `os.NewFile` so Go registers it with the runtime poller and cancellation interrupts the pending read. After terminal acknowledgement, route process waiting through the existing bounded cleanup/escalation path instead of an unbounded `child.Done()` wait. Preserve terminal ordering, fail-closed routing, process-group escalation, and exact-ticket recovery.

**Tech Stack:** Go 1.26, Unix inherited file descriptors, Go runtime poller, `context`, existing supervisor process protocol, existing supervisor cleanup/recovery machinery, Incus live acceptance tests.

## Global Constraints

- Work only in the isolated `fix/child-liveness-shutdown` worktree and branch.
- Follow strict TDD: add each regression first, run it against unchanged production code, and record the expected failure before implementing.
- Descriptor numbers remain bootstrap 3, liveness 4, report 5, and terminal acknowledgement 6.
- Terminal reports remain exact, single-use, and explicitly acknowledged before child teardown.
- A terminal result remains provisional until the exact child process exits and cleanup completes.
- Unexpected descendant unavailability and `/v1/tree` routing remain fail-closed.
- Process-group escalation and exact-ticket recovery remain limited to the admitted child and run only after confirmed process exit.
- Add no dependency and perform no unrelated refactor.

---

## File Structure

- `internal/supervisor/process/liveness.go` — prepares inherited protocol descriptors and coordinates child runtime/liveness shutdown.
- `internal/supervisor/process/process_test.go` — real-process protocol regressions for acknowledged completion and descriptor setup failure.
- `internal/supervisor/node.go` — creates a child, acknowledges its terminal report, and synchronously reaps it.
- `internal/supervisor/children_test.go` — supervisor-level regression for bounded post-terminal escalation, recovery, and registry removal.
- `docs/architecture/session-supervisor.md` — records the pollable liveness and bounded terminal-settlement invariants.
- `cmd/session.go` — preserves nil semantics for the optional inherited root startup-status writer.
- `cmd/session_test.go` — covers direct root startup without a status descriptor.

### Task 1: Make Normal Child Completion Interrupt the Liveness Read

**Files:**
- Modify: `internal/supervisor/process/liveness.go:39-95`
- Modify: `internal/supervisor/process/process_test.go:345-557`

**Interfaces:**
- Consumes: fixed inherited descriptor constants `BootstrapFD`, `LivenessFD`, `ReportFD`, and `TerminalAckFD`; `RunInheritedChild`; `Spawner`.
- Produces: `prepareInheritedLivenessDescriptor(fd int) error`, called before `os.NewFile` for descriptor 4; unchanged public child process protocol.

- [ ] **Step 1: Add a real-process regression for acknowledged completion while parent liveness stays open**

Add a helper branch and parent test alongside `TestParentStopUnblocksRealChildWaitingForTerminalAcknowledgement`:

```go
func TestAcknowledgedTerminalChildExitsWhileParentLivenessRemainsOpen(t *testing.T) {
	if os.Getenv("KANEDIAS_TERMINAL_EXIT_HELPER") == "1" {
		err := RunInheritedChild(context.Background(), BootstrapFD, LivenessFD, ReportFD, TerminalAckFD, func(_ context.Context, bootstrap Bootstrap, reporter *Reporter) error {
			info, err := os.Stat(bootstrap.SocketPath)
			if err != nil {
				return err
			}
			stat := info.Sys().(*syscall.Stat_t)
			ticket := provision.RecoveryTicket{
				SessionID: bootstrap.SessionID, ParentID: bootstrap.ParentID, RootID: bootstrap.RootID,
				Pool: "pool", Instance: "session-" + bootstrap.SessionID, Volume: "workspace-" + bootstrap.SessionID,
				SocketPath: bootstrap.SocketPath, Socket: provision.SocketIdentity{Device: uint64(stat.Dev), Inode: stat.Ino},
				Kind: bootstrap.Request.Kind, Context: bootstrap.Request.Context, WorkerType: bootstrap.Request.WorkerType,
			}
			if err := reporter.Ownership(ticket); err != nil {
				return err
			}
			if err := reporter.Ready(bootstrap.SocketPath); err != nil {
				return err
			}
			return reporter.Read(contract.ReadChildResult{
				Kind: contract.ChildKindRead, WorkerType: bootstrap.Request.WorkerType,
				SessionID: bootstrap.SessionID, Output: "done",
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	bootstrap := validBootstrap(t)
	serveTree(t, bootstrap.SocketPath, bootstrap.SessionID, bootstrap.ParentID, bootstrap.RootID)
	script := filepath.Join(t.TempDir(), "helper.sh")
	contents := "#!/bin/sh\nexec \"$KANEDIAS_TERMINAL_EXIT_TEST_BINARY\" -test.run '^TestAcknowledgedTerminalChildExitsWhileParentLivenessRemainsOpen$'\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KANEDIAS_TERMINAL_EXIT_HELPER", "1")
	t.Setenv("KANEDIAS_TERMINAL_EXIT_TEST_BINARY", os.Args[0])
	child, err := (Spawner{Executable: script, ProbeInterval: time.Millisecond}).Spawn(context.Background(), bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Kill() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := child.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	message, err := child.NextMessage(ctx)
	if err != nil || message.Type != MessageRead {
		t.Fatalf("terminal message = %#v, %v", message, err)
	}
	if err := child.AcknowledgeTerminal(message); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Done():
	case <-ctx.Done():
		t.Fatal("acknowledged child remained blocked on the still-open parent liveness pipe")
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := child.CloseReports(); err != nil {
		t.Fatal(err)
	}
}
```

Do not call `CloseLiveness` before asserting process exit; keeping the writer open is the regression condition.

- [ ] **Step 2: Run the exact test and verify RED**

Run:

```bash
go test ./internal/supervisor/process -run '^TestAcknowledgedTerminalChildExitsWhileParentLivenessRemainsOpen$' -count=1 -v
```

Expected: FAIL after the bounded deadline with `acknowledged child remained blocked on the still-open parent liveness pipe`.

- [ ] **Step 3: Add descriptor-setup failure coverage**

Add a focused unit test that creates a pipe, captures the read descriptor, closes it, calls the new `prepareInheritedLivenessDescriptor`, and requires a non-nil error containing `set inherited parent-liveness descriptor nonblocking`. This test defines the required bootstrap error rather than permitting silent fallback.

- [ ] **Step 4: Implement pollable inherited liveness setup**

In `liveness.go`, add:

```go
func prepareInheritedLivenessDescriptor(fd int) error {
	if err := syscall.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("set inherited parent-liveness descriptor nonblocking: %w", err)
	}
	return nil
}
```

In `RunInheritedChild`, after validating the fixed descriptor numbers and marking all four descriptors close-on-exec, call:

```go
if err := prepareInheritedLivenessDescriptor(livenessFD); err != nil {
	return err
}
```

Call it before `os.NewFile` so `os.NewFile` observes `O_NONBLOCK` and registers descriptor 4 with the runtime poller. Do not change the blocking mode of descriptors 3, 5, or 6.

- [ ] **Step 5: Verify GREEN and preserve existing protocol behavior**

Run:

```bash
go test ./internal/supervisor/process -run 'TestAcknowledgedTerminalChildExitsWhileParentLivenessRemainsOpen|TestParentStopUnblocksRealChildWaitingForTerminalAcknowledgement|TestParentLivenessEOFCascadesThroughRealChildAndGrandchild|TestMonitorParentLivenessUsesIdempotentStopPath' -count=1 -v
go test ./internal/supervisor/process -count=1
```

Expected: PASS. The new normal-completion test must exit without parent `CloseLiveness`; existing parent-death and cancellation tests must retain their behavior.

- [ ] **Step 6: Commit Task 1**

```bash
git add internal/supervisor/process/liveness.go internal/supervisor/process/process_test.go
git commit -m "fix: interrupt child liveness read on completion"
```

### Task 2: Bound and Reap a Wedged Terminal Child

**Files:**
- Modify: `internal/supervisor/node.go:414-434`
- Modify: `internal/supervisor/children_test.go`
- Modify: `docs/architecture/session-supervisor.md:340-380`

**Interfaces:**
- Consumes: `Node.childStopTimeout`, `Node.cleanupChild`, `ChildProcess` process-group escalation methods, `DirectChildRecoverer`.
- Produces: bounded post-acknowledgement settlement through `cleanupChild`; no new exported API.

- [ ] **Step 1: Add a stubborn terminal child test double**

In `children_test.go`, add a `terminalWedgeChildProcess` implementing `ChildProcess`. It must:

- return a valid recovery ticket;
- publish one valid terminal read message;
- count acknowledgement, terminal-ack closure, liveness closure, TERM, and KILL;
- leave `Done` open on acknowledgement, endpoint closure, liveness closure, and TERM;
- close `Done` only on KILL; and
- return a non-nil wait error after forced KILL so provisional success becomes `child_failed`.

Use atomics and `sync.Once` consistently with existing child test doubles.

- [ ] **Step 2: Add the bounded post-terminal regression**

Construct a child-creation node with a short `ChildStopTimeout`, a matching descendant snapshot, an `orderingRecoverer` bound to the stubborn child's `Done`, and a deterministic session ID. Invoke `CreateChild` in a goroutine and require it to return within one second.

Assert all of the following:

```go
if child.acknowledged.Load() != 1 { /* fail */ }
if child.liveness.Load() == 0 { /* fail */ }
if child.terminated.Load() != 1 { /* fail */ }
if child.killed.Load() != 1 { /* fail */ }
if recoverer.called.Load() != 1 { /* fail */ }
if node.children.get("child-terminal-wedge") != nil { /* fail */ }
```

Require the returned error to contain a typed `contract.Error` with code `contract.ErrorChildFailed`. Ensure the recovery assertion proves recovery ran only after `Done` closed.

On test timeout, call `child.Kill()` before failing so RED never leaks a goroutine.

- [ ] **Step 3: Run the exact test and verify RED**

Run:

```bash
go test ./internal/supervisor -run '^TestCreateChildBoundsAndReapsWedgedProcessAfterTerminalAcknowledgement$' -count=1 -v
```

Expected: FAIL because unchanged `CreateChild` waits indefinitely at its post-acknowledgement `select` until the test's timeout cleanup kills the child. At minimum, the escalation assertions must demonstrate that production did not bound the wait.

- [ ] **Step 4: Route terminal settlement through bounded cleanup**

Replace the unbounded post-acknowledgement `select` in `Node.CreateChild` with synchronous bounded cleanup:

```go
cleanupCtx, cancel := context.WithTimeout(ctx, node.childStopTimeout())
cleanupErr := node.cleanupChild(cleanupCtx, entry, false)
cancel()
if cleanupErr != nil && terminalErr == nil {
	terminalErr = contract.NewError(contract.ErrorChildFailed, "child process or resource cleanup failed")
}
return result, errors.Join(terminalErr, cleanupErr)
```

This deliberately uses the request context as the parent of the timeout: request cancellation accelerates cleanup, while `cleanupChild` still completes process escalation, wait, exact-ticket recovery, and registry removal synchronously. Do not acknowledge again and do not request a routed child stop after the terminal report has already been accepted.

- [ ] **Step 5: Update the architecture invariant**

In `docs/architecture/session-supervisor.md`, extend the recursive failure/shutdown section to state:

- inherited child liveness descriptors are made nonblocking before `os.NewFile` so context cancellation interrupts pending reads; and
- after exact terminal acknowledgement, the parent bounds real-process settlement and escalates through the admitted process group before exact-ticket recovery and registry removal.

Keep the existing ordering and ownership language intact.

- [ ] **Step 6: Verify Task 2 GREEN**

Run:

```bash
go test ./internal/supervisor -run 'TestCreateChildBoundsAndReapsWedgedProcessAfterTerminalAcknowledgement|TestCreateChildFailureWaitsForProcessExitBeforeReturningAndRemoval|TestCreateChildCancellationStopsAndSynchronouslyCleansChild|TestDirectChildRecoveryRunsAfterCrashAndForcedEscalationExit' -count=1 -v
go test ./internal/supervisor -count=1
```

Expected: PASS. The new test must show acknowledgement before bounded escalation, KILL before recovery, registry removal before return, and typed failure for forced cleanup.

- [ ] **Step 7: Commit Task 2**

```bash
git add internal/supervisor/node.go internal/supervisor/children_test.go docs/architecture/session-supervisor.md
git commit -m "fix: bound terminal child settlement"
```

### Task 3: Preserve Optional Root Startup-Status Nil Semantics

**Files:**
- Modify: `cmd/session_test.go`
- Modify: `cmd/session.go`

**Interfaces:**
- Consumes: optional hidden `--status-fd`; `SessionOptions.RootStatus io.WriteCloser`.
- Produces: a genuinely nil `RootStatus` when no status descriptor is inherited; unchanged ownership and idempotent closure when it is present.

- [ ] **Step 1: Add a direct-session regression**

Add a focused command test that runs `session --socket /tmp/root.sock` without `--status-fd`, captures `SessionOptions`, and requires `options.RootStatus == nil` inside the `runSupervisor` stub.

- [ ] **Step 2: Run the exact test and verify RED**

Run:

```bash
go test ./cmd -run '^TestSessionWithoutStatusDescriptorPassesNilRootStatus$' -count=1 -v
```

Expected: FAIL because assigning the nil concrete `*onceFile` to `SessionOptions.RootStatus` creates a non-nil interface.

- [ ] **Step 3: Normalize the optional writer at the command boundary**

Before constructing `SessionOptions`, copy `rootStatus` into an `io.WriteCloser` only when the concrete pointer is non-nil. Pass that normalized interface to `RootStatus`. Do not change the inherited descriptor validation, ownership, close-on-exec, or idempotent-close behavior.

- [ ] **Step 4: Verify focused GREEN and existing descriptor behavior**

Run:

```bash
go test ./cmd -run 'TestSessionWithoutStatusDescriptorPassesNilRootStatus|TestSessionStartupDescriptorsCloseOnRuntimeError|TestSessionStartupDescriptorsCloseOnBootstrapDecodeError' -count=1 -v
go test ./cmd -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit Task 3**

```bash
git add cmd/session.go cmd/session_test.go docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md docs/superpowers/plans/2026-08-10-child-liveness-shutdown.md
git commit -m "fix: preserve optional root startup status"
```

### Task 4: Keep Recursive Live-Acceptance Sockets Within the Unix Path Bound

**Files:**
- Modify: `internal/supervisor/live_incus_test.go`
- Modify: `docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md`

**Interfaces:**
- Consumes: the existing private `shortSocketDir` acceptance helper and fixed child socket basename.
- Produces: short paths for manual recursive root and descendant sockets; unchanged long-lived artifact locations.

- [ ] **Step 1: Add a non-destructive path-bound regression and verify RED**

Require manual recursive root and maximum-length child socket paths to be absolute, outside the deep artifact directory, below `syscall.RawSockaddrUnix`'s path bound, and beneath a mode-0700 directory. Run:

```bash
go test -tags incus ./internal/supervisor -run '^TestRecursiveAcceptanceUsesShortSocketPaths$' -count=1 -v
```

Expected: FAIL until the recursive socket-path helpers exist. The live predecessor failed with a 110-byte path and `exceeds the platform address bound for the Unix listener`.

- [ ] **Step 2: Reuse the private short socket directory**

Track one recursive socket directory per harness. Start manual roots there so deterministic descendant sockets are siblings in the same short directory. Keep executable, event, process, and Incus snapshots under `runDir`. Point all recursive descendant-removal assertions at the actual socket directory. Do not change production naming.

- [ ] **Step 3: Verify focused GREEN and rerun live acceptance**

Run the focused path test, the hermetic supervisor package with the `incus` tag, and then the exact authorized live recursive acceptance command. Require exact baseline cleanup on both pass and failure.

- [ ] **Step 4: Commit Task 4**

```bash
git add internal/supervisor/live_incus_test.go docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md docs/superpowers/plans/2026-08-10-child-liveness-shutdown.md
git commit -m "test: keep recursive acceptance sockets short"
```

### Task 5: Wait for Bound Active Child Snapshots in Live Acceptance

**Files:**
- Modify: `internal/supervisor/live_incus_test.go`
- Modify: `docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md`

- [ ] **Step 1: Add and run the lifecycle-predicate regression**

Add a focused test requiring the child visibility predicate to reject kind/context-matching but unbound `starting` and `running` snapshots, accept bound `ready`, `running`, and `awaiting_handoff` snapshots, and reject terminal states. Run it with the `incus` build tag and observe RED before correcting the predicate helper.

- [ ] **Step 2: Gate read and write assertions on binding and active lifecycle**

Use the helper in both fresh-read and forked-writer visibility polls. Do not hide failed lifecycle states or weaken identity matching. Wait through `starting`, but do not require observing the brief `ready` state before the child enters `running`; require the Pi session ID, session file, and complete model binding used by subsequent assertions and routed control.

- [ ] **Step 3: Verify focused GREEN and rerun the full live lifecycle**

Run the focused predicate test, the tagged hermetic supervisor package, and the exact authorized live recursive acceptance. External baseline resources must not be modified by cleanup.

- [ ] **Step 4: Commit Task 5**

```bash
git add internal/supervisor/live_incus_test.go docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md docs/superpowers/plans/2026-08-10-child-liveness-shutdown.md
git commit -m "test: wait for bound live child snapshots"
```

### Task 6: Prepare the Disposable Writer Checkout in Live Acceptance

**Files:**
- Modify: `internal/supervisor/live_incus_test.go`
- Modify: `docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md`

- [ ] **Step 1: Add and run the workspace regression**

Add a focused test deriving `WorkspaceStart{Repository: "sklarsa/kanedias-testing", Checkout: "kanedias-testing"}` and `/workspace/repos/kanedias-testing` from the configured slug. Observe RED before adding the derivation helper.

- [ ] **Step 2: Keep the manual root's default workspace**

Do not bootstrap a selected workspace that is absent from the seed volume: production correctly treats `WorkspaceStart` as validated selection rather than cloning. Continue exercising the direct root path with no bootstrap/status descriptors and socket polling for readiness.

- [ ] **Step 3: Materialize the run-local writer checkout**

Immediately before the writer phase, derive the canonical checkout, detect an existing `.git` directory, or clone the already-preflighted disposable remote as the managed `kanedias` user into the root's copied workspace volume. Run checkout Git inspection under the same managed-user environment. The forked child inherits the checkout; the seed remains untouched. Preserve origin preflight and all handoff verification.

- [ ] **Step 4: Verify focused GREEN and rerun live acceptance**

Run the workspace helper regression, tagged hermetic supervisor package, and full authorized recursive acceptance.

- [ ] **Step 5: Commit Task 6**

```bash
git add internal/supervisor/live_incus_test.go docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md docs/superpowers/plans/2026-08-10-child-liveness-shutdown.md
git commit -m "test: prepare recursive acceptance checkout"
```

### Task 7: Add a Narrow Live Child-Liveness Gate

**Files:**
- Modify: `internal/supervisor/live_incus_test.go`
- Modify: `docs/superpowers/specs/2026-08-10-child-liveness-shutdown-design.md`

- [ ] Add an explicitly authorized live test that builds the reviewed checkout, starts the owned proxy and one manual root, executes `exerciseFreshRead`, gracefully stops the root, verifies root resource/socket disappearance, and restores the exact baseline.
- [ ] Keep the monolithic recursive test unchanged for writer/nested stress, but do not let failures in those later phases obscure the liveness-specific result.
- [ ] Run the tagged hermetic package and the narrow authorized test with the disposable local-worker configuration.
- [ ] Commit as `test: isolate live child liveness acceptance`.

## Final Verification

After all task reviews are clean, run from the final branch state:

```bash
gofmt -w internal/supervisor/process/liveness.go internal/supervisor/process/process_test.go internal/supervisor/node.go internal/supervisor/children_test.go
git diff --check origin/main...HEAD
go test ./internal/supervisor/process -count=1
go test ./internal/supervisor -count=1
go test -race ./internal/supervisor/process ./internal/supervisor -count=1
go test ./... -count=1
node --test internal/server/web/*.test.js
make lint
make build
```

Load the disposable live-test environment from the main checkout without copying credentials into the worktree, then run the exact live recursive acceptance test from the reviewed worktree:

```bash
set -a
. /home/steven/source/github/kanedias/.env
set +a
go test -tags incus ./internal/supervisor -run '^TestLiveRecursiveSupervisorAcceptance$' -count=1 -v
```

The live result must show a real fresh read child returns its terminal output, exits, removes its socket/resources and registry entry, and leaves the root `/v1/tree` healthy. If prerequisites reject the run before destructive work, retain the exact diagnostic and do not represent it as a pass.

Package the complete `origin/main...HEAD` diff for an independent concurrency/lifecycle review and a second adversarial error-semantics/coverage review. Fix all Critical and Important findings, re-run affected tests, perform a scoped re-review, then repeat the full final verification before opening or updating the PR.
