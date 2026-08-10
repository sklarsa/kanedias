# Child Liveness Shutdown Design

## Goal

Guarantee that a supervised child process exits promptly after its terminal result is acknowledged, and guarantee that a parent eventually reaps a terminal child even if the child wedges during final shutdown.

The fix must prevent a completed delegation from remaining as `toolCalls: 1, toolResults: 0`, prevent the removed child socket from poisoning the root's `/v1/tree`, and preserve the supervisor's existing fail-closed routing and ownership rules.

## Root Cause

The child receives the parent-liveness pipe as inherited descriptor 4. `RunInheritedChild` reconstructs it with `os.NewFile`, but the inherited descriptor is in blocking mode. On Unix, `os.NewFile` only registers an inherited descriptor with the Go runtime poller when the descriptor is already nonblocking.

After the child runtime returns, `RunInheritedChild` cancels its context and waits for `MonitorParentLiveness` to finish. The monitor closes the liveness file and waits for its read goroutine, but that goroutine is blocked in a raw pipe `read`. Closing a non-pollable `os.File` does not interrupt that in-flight read. The direct parent still owns the pipe's write end and waits for `child.Done()`, producing a cycle:

1. parent waits for child process exit;
2. child waits for liveness monitor exit;
3. liveness monitor waits for the blocking read;
4. blocking read waits for the parent to close the writer.

The child has already acknowledged its terminal result and can remove its Incus resources and Unix socket before entering this deadlock. The parent therefore retains a live `childEntry` whose socket no longer exists. Recursive `/v1/tree` construction fails closed on that unavailable child, leaving the root stale and preventing the original `delegate_session` HTTP request from returning its result.

## Chosen Approach

Use two complementary lifecycle protections.

### Pollable inherited liveness descriptor

Before constructing the child-side `os.File`, set inherited liveness descriptor 4 to nonblocking mode. `os.NewFile` will then register the pipe with the Go runtime poller. Context cancellation can close the file, wake the pending read, and let `MonitorParentLiveness` and `RunInheritedChild` return without requiring the parent to close its writer first.

Failure to enable nonblocking mode is a child bootstrap failure. The runtime must not silently fall back to the known-deadlocking behavior.

The wire protocol remains unchanged: descriptor numbers, EOF-as-parent-death, close-on-exec behavior, and the rule that data on the liveness pipe is invalid all remain intact.

### Bounded post-terminal reaping

After a parent ingests and acknowledges a valid terminal report, it must not wait indefinitely on `child.Done()`. The terminal path will use the existing bounded child cleanup and escalation machinery so that a child which still fails to exit is given the configured cleanup interval and then has liveness closed, receives process-group termination, and finally receives process-group kill if necessary.

Recovery continues to run only after the exact admitted child process exits. The child registry entry is removed only after process wait, descriptor closure, descendant-client closure, and ownership-ticket recovery complete. A cleanup failure converts the provisional terminal success into `child_failed`, preserving the rule that successful delivery is not final until real process exit and cleanup.

## Scope

### In scope

- Pollable child-side parent-liveness descriptor setup.
- Prompt context cancellation of the liveness monitor after normal child runtime completion.
- A bounded parent wait after terminal acknowledgement.
- Existing TERM/KILL escalation and exact-ticket recovery on a wedged terminal child.
- Automated process-level, supervisor-level, race, full-suite, lint, build, and live delegation validation.
- Any narrowly required comments or architecture-document corrections.
- The approved validation-blocker correction described below, which is required to start a manual root on current `origin/main` and execute the live regression.

### Out of scope

- Making `/v1/tree` omit or tolerate unexpectedly unavailable children.
- Weakening fail-closed descendant routing.
- Changing descriptor numbers or replacing the inherited-pipe protocol.
- Changing terminal acknowledgement ordering.
- Returning a terminal result before the child process and owned resources are settled.
- General orphan discovery or host-crash reconciliation.

## Lifecycle and Error Semantics

### Successful child

1. Child produces a terminal read result or accepted write handoff.
2. Parent validates the exact terminal report.
3. Parent marks descendant event-stream closure expected.
4. Parent acknowledges the exact report.
5. Child tears down its local session, resources, listener, and runtime.
6. Child cancellation interrupts the pollable liveness read and the process exits.
7. Parent observes process exit, performs exact-ticket recovery as needed, removes the registry entry, and returns the terminal result to the blocked delegation request.

### Wedged child after acknowledgement

1. Steps 1–4 above complete.
2. The child does not exit within the bounded cleanup interval.
3. Parent closes protocol endpoints and escalates through process-group TERM and KILL using the existing cleanup path.
4. Parent waits for real process exit, performs exact-ticket recovery, and removes the registry entry.
5. The delegation returns `child_failed`; it does not report a provisional success whose child failed to settle.

### Parent death or cancellation

Parent-side liveness closure still yields EOF in the child and recursively initiates shutdown. Request cancellation still performs synchronous bounded child cleanup. Terminal acknowledgement remains an explicit separate protocol boundary and is never synthesized by cancellation.

## Testing

### Exact process regression

Add a real spawned-child test that runs `RunInheritedChild` through the production fixed descriptors. The child runner publishes ownership/readiness, sends a terminal read result, and returns after acknowledgement. The parent acknowledges the terminal report but intentionally keeps the liveness writer open. Before the fix, the child remains blocked; after the fix, it must exit promptly and cleanly without `CloseLiveness`.

The test must use a deadline and kill cleanup so a regression fails deterministically without hanging the suite.

### Bootstrap failure coverage

Exercise the liveness-descriptor setup seam so failure to make descriptor 4 nonblocking returns a clear bootstrap error rather than entering the child runtime.

### Bounded parent cleanup regression

Use a child-process test double that publishes a valid terminal result but refuses to exit normally. Configure a short `ChildStopTimeout` and assert:

- the terminal report is acknowledged first;
- cleanup does not remain unbounded;
- liveness closure and TERM/KILL escalation occur;
- exact-ticket recovery runs only after process exit;
- the child registry entry is removed; and
- the provisional terminal result is returned as `child_failed` when forced cleanup reports failure.

### Verification commands

Run fresh from the final branch state:

```bash
go test ./internal/supervisor/process -count=1
go test ./internal/supervisor -count=1
go test -race ./internal/supervisor/process ./internal/supervisor -count=1
go test ./... -count=1
make lint
make build
```

Run the narrow live Incus supervisor delegation test or acceptance target that exercises a real read child, terminal acknowledgement, process exit, socket removal, registry removal, and root `/v1/tree` recovery. The live test must use disposable resources and retain diagnostic evidence if environmental prerequisites prevent execution.

## Approved Validation-Blocker Addendum

Live validation against the rebased branch exposed a root-startup regression introduced on current `origin/main`, before child delegation or the liveness fix ran. `newSessionCommand` stores the absent `--status-fd` as a nil `*onceFile`, then assigns that pointer to the `io.WriteCloser` field `SessionOptions.RootStatus`. The resulting interface is non-nil. After a successful root start, `runSupervisorWithBrokerFactory` therefore tries to encode startup status through the nil pointer, panics, and leaves the already-provisioned root resources behind.

Normalize the optional writer at the command boundary: assign `SessionOptions.RootStatus` only when the concrete `*onceFile` is non-nil. Preserve the interface type and the existing idempotent descriptor ownership for real status descriptors. Add a command-level regression proving a direct `session` invocation without `--status-fd` forwards a genuinely nil `RootStatus`, while existing descriptor-forwarding and closure tests preserve the inherited-manager path. This adjacent correction is approved because it is necessary to execute the required live lifecycle validation and prevents a real root-startup resource leak.

## Alternatives Rejected

### Only close the parent liveness writer after acknowledgement

This would break the observed cycle, but it conflates normal child completion with the parent-death signal and leaves child-side context cancellation unable to interrupt its own monitor. The child runtime should be able to settle independently after acknowledgement.

### Add a timeout without fixing the pipe

This bounds the outage but makes every successful child wait for escalation. It treats the deadlock as expected cleanup instead of fixing it.

### Make `/v1/tree` tolerate a missing child socket

This would improve root availability while hiding an inconsistent ownership state. The tree and routing APIs intentionally fail closed when a registered live descendant cannot be attested. Correct process exit and bounded reaping restore consistency without weakening that boundary.

### Replace the liveness pipe protocol

A socketpair, eventfd, or separate polling loop could provide cancellation, but each changes more protocol and platform surface than needed. Making the inherited pipe explicitly pollable uses documented Go `os.NewFile` behavior and preserves the current protocol.

## Security and Ownership Invariants

- No new network endpoint or trust boundary is introduced.
- Protocol descriptors remain fixed, private, and close-on-exec.
- Child terminal reports remain exact, single-use, and explicitly acknowledged.
- Process-group escalation remains limited to the exact spawned child group.
- Recovery remains limited to deterministic resources in the exact pre-published ownership ticket and runs only after confirmed process exit.
- Unexpected descendant unavailability remains fail-closed.
